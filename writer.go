package erofs

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"slices"

	"github.com/forkcloser/erofs/internal/builder"
	"github.com/forkcloser/erofs/internal/disk"
)

// tailWindow is the most of a source image one read for an inline end
// takes, the end itself, at most a block, and those that follow it; and
// tailReach the least it takes once an end has followed another.
const (
	tailWindow = 256 << 10
	tailReach  = 4 << 10
)

// maxBlockSize is the largest block size we support. EROFS images with
// larger block sizes are unmountable on common platforms (aarch64 caps
// page size at 64 KiB) and the reader rejects BlkSizeBits > 16.
const maxBlockSize = 1 << 16

// errMetaOverrun is a planLayout invariant broken at write time: the
// metadata written so far has run past the nid slot it had to end at.
var errMetaOverrun = errors.New("metadata overran nid slot")

// onlyWriter wraps an io.Writer to hide io.ReaderFrom so that
// io.CopyBuffer uses the caller-provided buffer instead of
// the destination's ReadFrom (which allocates its own).
type onlyWriter struct{ io.Writer }

// erofsWriter serializes EROFS metadata to an io.Writer.
type erofsWriter struct {
	entries     []*erofsEntry // all entries in NID order
	rootNid     uint64
	metaBlkAddr uint32
	// The shared xattr area: the attributes more than sharedXattrOver
	// inodes carry, stored once, in blocks of their own between the data
	// and the metadata. shared is the area in its order, sharedSize its
	// bytes, and sharing says of an attribute where it is there. bodyIDs
	// and bodyOwn are one inode's references and own entries (xattrBody).
	shared       []xattr
	sharedSize   int
	sharing      xattrSharing
	xattrHead    [disk.SizeXattrEntry]byte // the head of the entry being written (writeXattrEntry)
	xattrBlkAddr uint32
	bodyIDs      []uint32
	bodyOwn      []xattr
	totalInodes  uint64
	buildTime    uint64
	buildTimeNs  uint32
	devices      []uint64 // per-device block counts (one slot per entry)
	blockSize    int
	// noInlineData keeps every regular file's data in whole blocks: no
	// tail packed beside the inode.
	noInlineData bool
	chunkBits    uint8  // log2(chunkSize / blockSize); chunkSize = blockSize << chunkBits
	copyBuf      []byte // reusable buffer for io.CopyBuffer
	// tailWin is the window of image tailOf at tailAt that the inline ends
	// of a metadata-only copy's files are read from (inlineEnd).
	tailWin  []byte
	tailOf   *image
	tailAt   int64
	zeroBuf  []byte                       // blockSize-length zero buffer for padding
	dirBuf   []byte                       // reusable scratch for one dirent block
	inodeBuf [disk.SizeInodeExtended]byte // scratch buffer for writeInode
}

// inodeSize returns the on-disk inode header size for e.
func inodeCoreSize(e *erofsEntry) int {
	if e.compact {
		return disk.SizeInodeCompact
	}

	return disk.SizeInodeExtended
}

// inodeSlotAlign rounds an inode's on-disk size up to whole inode slots.
func inodeSlotAlign(size int) int {
	return (size + disk.SizeInodeSlot - 1) &^ (disk.SizeInodeSlot - 1)
}

// entryChunkBits returns the chunk bits for a specific entry.
// Contiguous entries use a larger chunk size to minimize chunk indexes.
func (w *erofsWriter) entryChunkBits(e *erofsEntry) uint8 {
	if e.chunkBits > 0 {
		return e.chunkBits
	}

	return w.chunkBits
}

// entryChunkSize returns the chunk size in bytes for a specific entry.
func (w *erofsWriter) entryChunkSize(e *erofsEntry) int {
	return w.blockSize << w.entryChunkBits(e)
}

// maxChunkIndexEntries backstops the chunk-index map a single inode may
// carry, bounding it to 1 GiB of index entries.
//
// This is deliberately far above any realistic file — 512 GiB at a 4 KiB
// chunk size — because it is only a sanity backstop, not the primary defense.
// Untrusted images are bounded much more tightly at ingestion, where
// copyFromImage requires the declared map to actually be present in the
// source. This limit exists so that an overflowed or absurd size can never
// reach the in-memory metadata buffer, which the writer grows without bound.
const maxChunkIndexEntries = 1 << 27

// chunkCount returns the number of chunk-index entries e requires.
//
// The arithmetic is done in uint64 and clamped: e.size may come from an
// untrusted inode, where the obvious int conversion either overflows to a
// negative count or yields a count whose index map is far larger than any
// real filesystem. The clamped value is over the limit by one, so
// checkLimits rejects the entry before anything is written.
func (w *erofsWriter) chunkCount(e *erofsEntry) int {
	// #nosec G115 -- a block of at most 1<<16 bytes shifted by at most 31 chunk bits: positive, below 1<<47
	cs := uint64(w.entryChunkSize(e))
	if cs == 0 {
		return 0
	}

	n := e.size / cs
	if e.size%cs != 0 {
		n++
	}

	if n > maxChunkIndexEntries {
		return maxChunkIndexEntries + 1
	}

	return int(n)
}

// checkLimits rejects entries the on-disk format cannot represent. It runs
// before any serialization, so an entry that would be silently truncated into
// a narrower field never reaches the metadata buffer.
//
//nolint:gocognit // one loop over the entries, each on-disk limit checked in place
func (w *erofsWriter) checkLimits() error {
	for _, e := range w.entries {
		// A zero-length target is not a resolvable symlink: readers fold it
		// away and restart the walk at the root, so any path through the link
		// silently loses its prefix. Symlink rejects it at the API boundary;
		// this catches the CopyFrom paths, where the source picks the target.
		if e.mode&disk.StatTypeMask == disk.StatTypeSymlink && e.symTarget == "" {
			return fmt.Errorf("mkfs: %s: empty symlink target: %w", e.path, ErrInvalid)
		}

		if e.layout == disk.LayoutChunkBased && w.chunkCount(e) > maxChunkIndexEntries {
			return fmt.Errorf("mkfs: %s: chunk index for a %d byte file exceeds the %d entry limit: %w",
				e.path, e.size, int64(maxChunkIndexEntries), ErrInvalid)
		}
		// A chunk index stores the block address in 32 bits; the 48-bit
		// format that widens it is not implemented end to end. Nothing
		// upstream bounds a chunk's start block, so reject what would
		// otherwise be truncated into an image that reads back as
		// phys mod 2^32.
		for _, c := range e.chunks {
			if c.PhysicalBlock == builder.NullPhysicalBlock {
				continue
			}

			if end := c.PhysicalBlock + uint64(c.Count); end > math.MaxUint32 {
				return fmt.Errorf("mkfs: %s: chunk covering blocks %d..%d needs 48-bit addressing: %w",
					e.path, c.PhysicalBlock, end, ErrNotImplemented)
			}
		}
		// Setxattr validates at the API boundary, but xattrs also arrive
		// through CopyFrom, where the source picks the lengths.
		for _, x := range e.xattrs {
			if err := validateXattr(x.name, x.value); err != nil {
				return fmt.Errorf("mkfs: %s: %w", e.path, err)
			}
		}

		if e.xattrSize > 0 && xattrICount(e.xattrSize) > maxXattrICount {
			return fmt.Errorf(
				"mkfs: %s: xattr area of %d bytes needs more than the %d entries i_xattr_icount can hold: %w",
				e.path,
				e.xattrSize,
				maxXattrICount,
				ErrInvalid,
			)
		}
	}
	// Block addresses are 32 bits wide: i_u's raw_blkaddr, MetaBlkAddr and
	// the superblock's block count. Nothing upstream bounds how much flat
	// data a source packs, and past 2^32 blocks the addresses wrap, so data
	// and metadata alias each other's blocks. Summed entry by entry, so the
	// count stops at the limit long before it could overflow.
	blockSize := int64(w.blockSize)
	blocks := int64(w.sbAreaBlocks()) + int64(w.sharedBlocks()) + (int64(w.metadataBytes())+blockSize-1)/blockSize

	for _, entry := range w.entries {
		ds := int64(w.flatPlainDataSize(entry))

		blocks += ds / blockSize
		if ds%blockSize != 0 {
			blocks++
		}

		if entry.tail.n > 0 {
			blocks++
		}

		if blocks > math.MaxUint32 {
			return fmt.Errorf("mkfs: %s: image needs more than the %d blocks a 32-bit block address can name: %w",
				entry.path, uint64(math.MaxUint32), ErrInvalid)
		}
	}

	return nil
}

// runChunkBits returns the largest chunkBits at which e's index still says
// what its chunks say: an index entry maps one whole chunk from its first
// block, so a chunk may not hold the place where a run of e's blocks ends
// and another begins, a hole after data, data on another device, or blocks
// that do not follow. Every such place is a multiple of the chunk size;
// with none, one chunk holds the file. A file's holes then cost an entry a
// chunk, not an entry a block.
func (w *erofsWriter) runChunkBits(e *erofsEntry) uint8 {
	fit := w.minChunkBits(e.size)

	var (
		at   uint64 // the logical block the next run starts at
		prev *builder.Chunk
	)

	for i := range e.chunks {
		c := &e.chunks[i]
		if prev != nil && !follows(prev, c) {
			fit = min(fit, alignedBits(at))
		}

		at += uint64(c.Count)
		prev = c
	}
	// Past the chunks the file is its tail, a block of this image that
	// follows nothing, or else a hole, which follows a hole alone.
	// #nosec G115 -- the block size is positive, 512 to 1<<16
	blocks := (e.size + uint64(w.blockSize) - 1) / uint64(w.blockSize)
	if prev != nil && at < blocks && (e.tail.n > 0 || prev.PhysicalBlock != builder.NullPhysicalBlock) {
		fit = min(fit, alignedBits(at))
	}

	return fit
}

// follows reports whether c continues the run prev ends: a hole after a
// hole, or the next blocks of the same device.
func follows(prev, c *builder.Chunk) bool {
	if prev.PhysicalBlock == builder.NullPhysicalBlock || c.PhysicalBlock == builder.NullPhysicalBlock {
		return prev.PhysicalBlock == c.PhysicalBlock
	}

	return prev.DeviceID == c.DeviceID && prev.PhysicalBlock+uint64(prev.Count) == c.PhysicalBlock
}

// alignedBits is the largest chunkBits whose chunk size divides block.
func alignedBits(block uint64) uint8 {
	// #nosec G115 -- a count of bits, at most 64
	return uint8(bits.TrailingZeros64(block))
}

// minChunkBits returns the minimum chunkBits such that file size fits in
// one chunk (chunkSize >= size). Capped at 31 (LayoutChunkFormatBits max).
func (w *erofsWriter) minChunkBits(size uint64) uint8 {
	fit := w.chunkBits
	// #nosec G115 -- the block size is positive, 512 to 1<<16
	for uint64(w.blockSize)<<fit < size && fit < 31 {
		fit++
	}

	return fit
}

func (w *erofsWriter) write(out io.WriteSeeker) error {
	if err := w.checkLimits(); err != nil {
		return err
	}

	w.copyBuf = make([]byte, 256*1024) //nolint:mnd // 256 KiB shared io.CopyBuffer buffer

	return w.writeSeekable(out)
}

// writeSeekable uses a data-first on-disk layout: block0 (placeholder),
// data blocks, metadata. After everything is written, it seeks back to
// write the real superblock. This matches how mkfs.erofs lays out
// streaming sources — data is written as it arrives, metadata last.
func (w *erofsWriter) writeSeekable(out io.WriteSeeker) error {
	// Data-first layout: sbArea, data blocks, metadata.
	// Set metaBlkAddr to a sentinel so assignDataBlocks uses data-first.
	w.metaBlkAddr = 0xFFFFFFFF
	w.assignDataBlocks()

	// Write placeholder superblock area.
	if _, err := out.Write(make([]byte, w.sbAreaSize())); err != nil {
		return err
	}

	// Stream data blocks directly to output.
	if err := w.writeDataBlocks(out); err != nil {
		return err
	}

	// The metadata goes out as it is serialized, never held whole: it is
	// every inode, attribute, directory and inline file of the image.
	meta := bufio.NewWriterSize(out, len(w.copyBuf))
	if err := w.writeSharedXattrs(meta); err != nil {
		return err
	}

	if err := w.writeMetadataInodes(meta); err != nil {
		return err
	}

	if err := meta.Flush(); err != nil {
		return err //nolint:wrapcheck // the output's own error, as every write of the image returns it
	}

	// Seek back and write the real block 0 (superblock).
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}

	return w.writeBlock0(out)
}

// direntBuf returns a zeroed scratch buffer of n bytes for serializing one
// dirent block. It is reused across blocks: a directory can hold thousands.
func (w *erofsWriter) direntBuf(n int) []byte {
	if cap(w.dirBuf) < n {
		w.dirBuf = make([]byte, n)
	}

	w.dirBuf = w.dirBuf[:n]
	clear(w.dirBuf)

	return w.dirBuf
}

// assignDataBlocks assigns data block addresses to flat-plain entries.
// For metadata-first layout, data follows metadata.
// For data-first layout, data starts after the superblock area.
func (w *erofsWriter) assignDataBlocks() {
	sbBlks := w.sbAreaBlocks()
	// #nosec G115 -- checkLimits bounded the image's block count to 32 bits
	if w.metaBlkAddr == uint32(sbBlks) {
		// Metadata-first: data blocks come after metadata.
		totalMetaBytes := 0

		for _, e := range w.entries {
			expectedOff := int(e.nid) * 32 // #nosec G115 -- planLayout made the nid from an int offset / 32

			sz := inodeSlotAlign(inodeCoreSize(e) + e.xattrSize + e.chunkPad + e.trailingSize)

			end := expectedOff + sz
			if end > totalMetaBytes {
				totalMetaBytes = end
			}
		}

		metaBlocks := (totalMetaBytes + w.blockSize - 1) / w.blockSize

		// #nosec G115 -- checkLimits bounded the image's block count to 32 bits
		w.assignFlatPlainAddrs(uint32(w.sbAreaBlocks() + metaBlocks))
	} else {
		// Data-first: data starts after superblock area. The shared xattr
		// area follows the data, and the metadata follows it.
		// #nosec G115 -- checkLimits bounded the image's block count to 32 bits
		w.metaBlkAddr = w.assignFlatPlainAddrs(uint32(w.sbAreaBlocks()))

		if blocks := w.sharedBlocks(); blocks > 0 {
			w.xattrBlkAddr = w.metaBlkAddr
			w.metaBlkAddr += uint32(blocks) // #nosec G115 -- checkLimits bounded the image's block count to 32 bits
		}
	}
}

// assignFlatPlainAddrs hands out consecutive block addresses from addr to
// every flat-plain entry with data, and returns the first block after them.
func (w *erofsWriter) assignFlatPlainAddrs(addr uint32) uint32 {
	for _, e := range w.entries {
		if ds := w.flatPlainDataSize(e); ds > 0 {
			e.dataBlkAddr = addr
			if e.chunksOfData {
				placeChunks(e.chunks, uint64(addr), 0)
			}
			// #nosec G115 -- checkLimits bounded the image's block count, and so every address, to 32 bits
			addr += uint32((ds + w.blockSize - 1) / w.blockSize)
		}

		if e.tail.n > 0 {
			e.tailBlkAddr = addr
			// The tail's chunk: the one block of this image, after the
			// chunks on the source's devices. It begins a chunk of its
			// own, whatever the chunk size: runChunkBits holds that to
			// what the blocks before the tail are a multiple of.
			e.chunks = append(e.chunks, builder.Chunk{PhysicalBlock: uint64(addr), Count: 1})
			addr++
		}
	}

	return addr
}

// sbAreaSize returns the number of bytes needed for the superblock area
// (blocks before metadata): 1024-byte pad + superblock + device slots,
// rounded up to block boundary.
func (w *erofsWriter) sbAreaSize() int {
	n := disk.SuperBlockOffset + disk.SizeSuperBlock
	if len(w.devices) > 0 {
		n += len(w.devices) * disk.SizeDeviceSlot
	}

	return ((n + w.blockSize - 1) / w.blockSize) * w.blockSize
}

// sbAreaBlocks returns the number of blocks occupied by the superblock area.
func (w *erofsWriter) sbAreaBlocks() int {
	return w.sbAreaSize() / w.blockSize
}

// metadataBytes computes the total size of the metadata area, including
// any zero-padding inserted to reach each inode's expected offset (NID * 32)
// and rounding each entry up to a 32-byte boundary.
func (w *erofsWriter) metadataBytes() int {
	curOff := 0

	for _, e := range w.entries {
		expectedOff := int(e.nid) * 32 // #nosec G115 -- planLayout made the nid from an int offset / 32
		if curOff < expectedOff {
			curOff = expectedOff
		}

		sz := inodeCoreSize(e) + e.xattrSize + e.chunkPad + e.trailingSize
		if rem := sz % 32; rem != 0 {
			sz += 32 - rem
		}

		curOff += sz
	}

	return curOff
}

func (w *erofsWriter) writeBlock0(buf io.Writer) error {
	sbArea := make([]byte, w.sbAreaSize())

	totalMetaBytes := w.metadataBytes()
	metaBlocks := (totalMetaBytes + w.blockSize - 1) / w.blockSize

	// Count data blocks.
	dataBlocks := 0

	for _, e := range w.entries {
		if ds := w.flatPlainDataSize(e); ds > 0 {
			dataBlocks += (ds + w.blockSize - 1) / w.blockSize
		}

		if e.tail.n > 0 {
			dataBlocks++
		}
	}

	totalBlocks := w.sbAreaBlocks() + w.sharedBlocks() + metaBlocks + dataBlocks

	var (
		featureIncompat uint32
		extraDevices    uint16
		devtSlotOff     uint16
	)

	if len(w.devices) > 0 {
		featureIncompat |= disk.FeatureIncompatDeviceTable
		extraDevices = uint16(len(w.devices)) // #nosec G115 -- checkDeviceCount caps the table at 65535
		devtSlotOff = uint16((disk.SuperBlockOffset + disk.SizeSuperBlock) / disk.SizeDeviceSlot)
	}
	// Keyed off the resolved layout, not off len(e.chunks): a metadata-only
	// entry with no chunk mappings still gets LayoutChunkBased, and an image
	// carrying chunk-based inodes has to declare the feature.
	for _, e := range w.entries {
		if e.layout == disk.LayoutChunkBased {
			featureIncompat |= disk.FeatureIncompatChunkedFile

			break
		}
	}

	sb := disk.SuperBlock{
		MagicNumber:     disk.MagicNumber,
		FeatureCompat:   disk.FeatureCompatSBChksum,
		BlkSizeBits:     blkBits(w.blockSize),
		RootNid:         uint16(w.rootNid), // #nosec G115 -- planLayout places the root first, at nid 0
		Inos:            w.totalInodes,
		BuildTime:       w.buildTime,
		BuildTimeNs:     w.buildTimeNs,
		Blocks:          uint32(totalBlocks), // #nosec G115 -- checkLimits bounded the block count to 32 bits
		MetaBlkAddr:     w.metaBlkAddr,
		XattrBlkAddr:    w.xattrBlkAddr,
		FeatureIncompat: featureIncompat,
		ExtraDevices:    extraDevices,
		DevtSlotOff:     devtSlotOff,
	}

	// Marshalled straight into sbArea. binary.Write reaches the same bytes
	// through reflection, at roughly two orders of magnitude the cost, and
	// wanted a fresh bytes.Buffer for the superblock and for every device
	// slot besides.
	sb.Marshal(sbArea[disk.SuperBlockOffset:])

	// Write device slots right after superblock.
	for i, blocks := range w.devices {
		if blocks > math.MaxUint32 {
			return fmt.Errorf("device %d block count %d exceeds 32-bit limit: %w", i+1, blocks, ErrInvalid)
		}

		devSlot := disk.DeviceSlot{
			Blocks: uint32(blocks),
		}
		off := disk.SuperBlockOffset + disk.SizeSuperBlock + i*disk.SizeDeviceSlot
		devSlot.Marshal(sbArea[off:])
	}
	// Last, over the finished block: the checksum covers the device slots in
	// that block and the padding after the superblock as well as its own
	// fields, as mkfs.erofs writes it and the kernel verifies it. The area
	// is at least the span long: a 512-byte block's span ends exactly at its
	// 1536 bytes.
	start := disk.SuperBlockOffset + checksumEnd
	binary.LittleEndian.PutUint32(
		sbArea[disk.SuperBlockOffset+4:],
		superblockChecksum(sbArea[start:start+checksumSpan(sb.BlkSizeBits)]),
	)

	_, err := buf.Write(sbArea)

	return err
}

// writeMetadataInodes writes inode metadata. Data block addresses must
// already be assigned on each entry before calling this method.
//
//nolint:gocognit // the metadata serialization: each inode's slot, xattrs, trailing data and padding, accounted in one place
func (w *erofsWriter) writeMetadataInodes(buf io.Writer) error {
	metaStart := 0

	for _, e := range w.entries {
		expectedOff := int(e.nid) * 32 // #nosec G115 -- planLayout made the nid from an int offset / 32
		// planLayout hands out nids as byte offsets, so the bytes written so
		// far must never have run past this entry's slot: if they have, every
		// following dirent nid points at misaligned garbage. Fail loudly
		// rather than emit an image that opens but decodes to nonsense.
		if expectedOff < metaStart {
			return fmt.Errorf("write inode for %s: %w %d by %d bytes",
				e.path, errMetaOverrun, e.nid, metaStart-expectedOff)
		}

		if expectedOff > metaStart {
			if _, err := buf.Write(w.zeroBuf[:expectedOff-metaStart]); err != nil {
				return err
			}

			metaStart = expectedOff
		}

		if err := w.writeInode(buf, e); err != nil {
			return fmt.Errorf("write inode for %s: %w", e.path, err)
		}

		if e.compact {
			metaStart += disk.SizeInodeCompact
		} else {
			metaStart += disk.SizeInodeExtended
		}

		// Write xattr area
		if e.xattrSize > 0 {
			if err := w.writeXattrs(buf, e); err != nil {
				return fmt.Errorf("write xattrs for %s: %w", e.path, err)
			}

			metaStart += e.xattrSize
		}

		// Write trailing data
		switch e.mode & disk.StatTypeMask {
		case disk.StatTypeReg:
			if e.layout == disk.LayoutChunkBased && (e.size > 0 || len(e.chunks) > 0) {
				// Align the chunk-index map to the chunk-index unit.
				if e.chunkPad > 0 {
					if _, err := buf.Write(w.zeroBuf[:e.chunkPad]); err != nil {
						return err
					}

					metaStart += e.chunkPad
				}

				if err := w.writeChunkIndexes(buf, e); err != nil {
					return fmt.Errorf("write chunks for %s: %w", e.path, err)
				}

				metaStart += e.trailingSize
			} else if e.layout == disk.LayoutFlatInline && e.size > 0 && e.data != nil {
				// #nosec G115 -- planLayout inlines only a size that fits in one block
				n, err := io.CopyBuffer(onlyWriter{buf}, io.LimitReader(e.data, int64(e.size)), w.copyBuf)
				if c, ok := e.data.(io.Closer); ok {
					_ = c.Close()
				}

				if err != nil {
					return fmt.Errorf("write inline data for %s: %w", e.path, err)
				}
				// The layout reserved e.size bytes here. A short source would
				// otherwise be padded out with the zero fill that aligns the
				// next inode, leaving the file silently truncated-with-NULs
				// instead of reporting the problem. The flat-plain path in
				// writeDataBlocks already rejects this.
				// #nosec G115 -- planLayout inlines only a size that fits in one block
				if n != int64(e.size) {
					return fmt.Errorf("write inline data for %s: short read: got %d bytes, expected %d: %w",
						e.path, n, e.size, io.ErrUnexpectedEOF)
				}

				metaStart += int(n)
			}
		case disk.StatTypeDir:
			if e.layout == disk.LayoutFlatInline {
				n, err := w.writeDirents(buf, e)
				if err != nil {
					return fmt.Errorf("write dirents for %s: %w", e.path, err)
				}

				metaStart += n
			}
		case disk.StatTypeSymlink:
			if e.layout == disk.LayoutFlatInline {
				if _, err := io.WriteString(buf, e.symTarget); err != nil {
					return fmt.Errorf("write symlink for %s: %w", e.path, err)
				}

				metaStart += len(e.symTarget)
			}
		}

		// Pad to 32-byte boundary
		inodeSize := disk.SizeInodeExtended
		if e.compact {
			inodeSize = disk.SizeInodeCompact
		}

		totalWritten := inodeSize + e.xattrSize + e.chunkPad + e.trailingSize
		if totalWritten%32 != 0 {
			padSize := 32 - (totalWritten % 32)
			if _, err := buf.Write(w.zeroBuf[:padSize]); err != nil {
				return err
			}

			metaStart += padSize
		}
	}

	// Pad metadata to full block boundary
	if metaStart%w.blockSize != 0 {
		padSize := w.blockSize - (metaStart % w.blockSize)
		if _, err := buf.Write(w.zeroBuf[:padSize]); err != nil {
			return err
		}
	}

	return nil
}

func (w *erofsWriter) writeInode(buf io.Writer, e *erofsEntry) error {
	var inodeData uint32

	switch e.mode & disk.StatTypeMask {
	case disk.StatTypeReg:
		if e.layout == disk.LayoutChunkBased {
			inodeData = disk.LayoutChunkFormatIndexes | uint32(w.entryChunkBits(e))
		} else if e.layout == disk.LayoutFlatPlain && e.size > 0 {
			inodeData = e.dataBlkAddr
		}
	case disk.StatTypeDir, disk.StatTypeSymlink:
		if e.layout == disk.LayoutFlatPlain {
			inodeData = e.dataBlkAddr
		}
	case disk.StatTypeChrdev, disk.StatTypeBlkdev, disk.StatTypeFifo, disk.StatTypeSock:
		inodeData = e.rdev
	}

	fileSize := w.inodeFileSize(e)

	b := &w.inodeBuf
	clear(b[:])

	if e.compact {
		binary.LittleEndian.PutUint16(b[0:2], inodeFormat(e.layout, true))
		binary.LittleEndian.PutUint16(b[2:4], xattrCount(e.xattrSize))
		binary.LittleEndian.PutUint16(b[4:6], e.mode)
		// #nosec G115 -- planLayout packs compact only when nlink is at most 0xFFFF
		binary.LittleEndian.PutUint16(b[6:8], uint16(e.nlink))
		// #nosec G115 -- planLayout packs compact only when i_size is at most 0xFFFFFFFF
		binary.LittleEndian.PutUint32(b[8:12], uint32(fileSize))
		binary.LittleEndian.PutUint32(b[16:20], inodeData)
		// #nosec G115 -- planLayout packs compact only when the uid is at most 0xFFFF
		binary.LittleEndian.PutUint16(b[24:26], uint16(e.uid))
		// #nosec G115 -- planLayout packs compact only when the gid is at most 0xFFFF
		binary.LittleEndian.PutUint16(b[26:28], uint16(e.gid))
		_, err := buf.Write(b[:disk.SizeInodeCompact])

		return err
	}

	binary.LittleEndian.PutUint16(b[0:2], inodeFormat(e.layout, false))
	binary.LittleEndian.PutUint16(b[2:4], xattrCount(e.xattrSize))
	binary.LittleEndian.PutUint16(b[4:6], e.mode)
	binary.LittleEndian.PutUint64(b[8:16], fileSize)
	binary.LittleEndian.PutUint32(b[16:20], inodeData)
	binary.LittleEndian.PutUint32(b[24:28], e.uid)
	binary.LittleEndian.PutUint32(b[28:32], e.gid)
	binary.LittleEndian.PutUint64(b[32:40], e.mtime)
	binary.LittleEndian.PutUint32(b[40:44], e.mtimeNs)
	binary.LittleEndian.PutUint32(b[44:48], e.nlink)
	_, err := buf.Write(b[:disk.SizeInodeExtended])

	return err
}

func (w *erofsWriter) writeXattrs(buf io.Writer, e *erofsEntry) error {
	w.xattrBody(e.xattrs, true)

	// XattrHeader: 4-byte name filter + 1-byte shared count + 7 reserved = 12 bytes
	var xhdr [12]byte
	binary.LittleEndian.PutUint32(xhdr[0:4], math.MaxUint32) // name filter unused
	xhdr[4] = uint8(len(w.bodyIDs))                          // #nosec G115 -- xattrBody shares at most maxSharedXattrs

	if _, err := buf.Write(xhdr[:]); err != nil {
		return err
	}

	// The references to the shared area come first, then the entries of
	// the inode's own (xattr.c: erofs_init_inode_xattrs).
	var id [4]byte
	for _, shared := range w.bodyIDs {
		binary.LittleEndian.PutUint32(id[:], shared)

		if _, err := buf.Write(id[:]); err != nil {
			return err
		}
	}

	for _, x := range w.bodyOwn {
		if err := w.writeXattrEntry(buf, x); err != nil {
			return err
		}
	}

	return nil
}

// writeXattrEntry writes one attribute as the format stores it, in an inode
// and in the shared area alike: the entry, the name after its prefix, the
// value, padded to four bytes.
func (w *erofsWriter) writeXattrEntry(buf io.Writer, x xattr) error {
	nameIndex, suffix := xattrSplit(x.name)

	// The writer's own four bytes: an array of this call's, handed to buf,
	// is one allocation for each entry written.
	xent := w.xattrHead[:]

	xent[0] = uint8(len(suffix)) // #nosec G115 -- checkLimits ran validateXattr: at most maxXattrNameLen
	xent[1] = nameIndex
	// #nosec G115 -- checkLimits ran validateXattr: at most maxXattrValueLen
	binary.LittleEndian.PutUint16(xent[2:4], uint16(len(x.value)))

	if _, err := buf.Write(xent); err != nil {
		return err
	}

	if _, err := io.WriteString(buf, suffix); err != nil {
		return err
	}

	if _, err := io.WriteString(buf, x.value); err != nil {
		return err
	}

	// Pad to 4-byte boundary
	entryLen := disk.SizeXattrEntry + len(suffix) + len(x.value)
	if entryLen%4 != 0 {
		if _, err := buf.Write(w.zeroBuf[:4-entryLen%4]); err != nil {
			return err
		}
	}

	return nil
}

// sharedBlocks is how many blocks the shared xattr area takes.
func (w *erofsWriter) sharedBlocks() int {
	return (w.sharedSize + w.blockSize - 1) / w.blockSize
}

// writeSharedXattrs writes the shared xattr area, padded to a block.
func (w *erofsWriter) writeSharedXattrs(buf io.Writer) error {
	for _, x := range w.shared {
		if err := w.writeXattrEntry(buf, x); err != nil {
			return err
		}
	}

	if pad := w.sharedBlocks()*w.blockSize - w.sharedSize; pad > 0 {
		if _, err := buf.Write(w.zeroBuf[:pad]); err != nil {
			return err
		}
	}

	return nil
}

// writeChunkIndexes writes chunk index entries for a regular file.
// Each index entry covers one logical chunk (chunkSize bytes).
//
//nolint:gocognit // one loop over logical chunks, stepping through the source chunks as it goes
func (w *erofsWriter) writeChunkIndexes(buf io.Writer, e *erofsEntry) error {
	cs := w.entryChunkSize(e)
	blocksPerChunk := cs / w.blockSize
	nchunks := w.chunkCount(e)

	// Null chunk index (no mapping): StartBlkHi=0xFFFF, DeviceID=0, StartBlkLo=NullAddr.
	var nullIdx [disk.SizeChunkIndex]byte
	binary.LittleEndian.PutUint16(nullIdx[0:2], math.MaxUint16)
	binary.LittleEndian.PutUint32(nullIdx[4:8], nullAddr)

	if len(e.chunks) > 0 {
		// Walk source chunks and emit one index per logical chunk.
		// Source chunks use block-granularity counts; we step by blocksPerChunk.
		// A chunk with PhysicalBlock == builder.NullPhysicalBlock is a hole:
		// emit nullIdx entries for its block span.
		var scratch [disk.SizeChunkIndex]byte

		ci := 0   // index into source chunks
		coff := 0 // block offset within current source chunk

		for range nchunks {
			if ci >= len(e.chunks) {
				if _, err := buf.Write(nullIdx[:]); err != nil {
					return err
				}

				continue
			}

			c := e.chunks[ci]
			if c.PhysicalBlock == builder.NullPhysicalBlock {
				// Hole chunk: emit a null index entry.
				if _, err := buf.Write(nullIdx[:]); err != nil {
					return err
				}
			} else {
				phys := c.PhysicalBlock + uint64(coff)
				// The first two bytes are the kernel's "advise" field, which
				// is defined to be zero unless the inode declares the 48-bit
				// chunk format. Nothing here sets that bit, so writing the
				// address's high half there produced an image the kernel and
				// this reader both resolve as phys mod 2^32. checkLimits
				// rejects anything that would need those bits.
				binary.LittleEndian.PutUint16(scratch[2:4], c.DeviceID)
				// #nosec G115 -- checkLimits rejected any chunk ending past math.MaxUint32
				binary.LittleEndian.PutUint32(scratch[4:8], uint32(phys))

				if _, err := buf.Write(scratch[:]); err != nil {
					return err
				}
			}

			coff += blocksPerChunk
			for ci < len(e.chunks) && coff >= int(e.chunks[ci].Count) {
				coff -= int(e.chunks[ci].Count)
				ci++
			}
		}
	} else {
		for range nchunks {
			if _, err := buf.Write(nullIdx[:]); err != nil {
				return err
			}
		}
	}

	return nil
}

// writeDirents writes EROFS directory entries packed into block-sized chunks.
//
//nolint:gocognit // packs dirents block by block: how many fit, then their serialization
func (w *erofsWriter) writeDirents(buf io.Writer, e *erofsEntry) (int, error) {
	type direntInfo struct {
		name     string
		nid      uint64
		fileType uint8
	}

	// Build the full entry list including "." and ".." then sort
	// alphabetically. EROFS requires dirents to be sorted within
	// each block; "." and ".." are not guaranteed to be first.
	allEnts := make([]direntInfo, 0, len(e.children)+2)
	allEnts = append(allEnts,
		direntInfo{".", e.nid, disk.FileTypeDir},
		direntInfo{"..", e.parentNid, disk.FileTypeDir},
	)

	for _, c := range e.children {
		allEnts = append(allEnts, direntInfo{
			name:     c.name,
			nid:      c.nid,
			fileType: c.erofsFileType,
		})
	}

	slices.SortFunc(allEnts, func(a, b direntInfo) int {
		return cmp.Compare(a.name, b.name)
	})

	totalWritten := 0

	i := 0
	for i < len(allEnts) {
		// Determine how many entries fit in this block
		start := i
		blockUsed := 0
		nameSize := 0

		for j := i; j < len(allEnts); j++ {
			headerSize := (j - start + 1) * disk.SizeDirent
			nameSize += len(allEnts[j].name)

			needed := headerSize + nameSize
			if needed > w.blockSize {
				break
			}

			blockUsed = needed
			i = j + 1
		}

		if i == start {
			// Single entry too large for a block (shouldn't happen)
			blockUsed = disk.SizeDirent + len(allEnts[i].name)
			i++
		}

		blockEnts := allEnts[start:i]
		blockHeaderSize := len(blockEnts) * disk.SizeDirent

		// Serialized into a block-sized scratch buffer and written once.
		// Writing each 12-byte header and each name separately is a syscall
		// apiece when out is an *os.File — invisible to an in-memory
		// benchmark, and the dominant cost when writing to disk.
		blk := w.direntBuf(blockUsed)
		nameOff := blockHeaderSize

		for j, de := range blockEnts {
			off := j * disk.SizeDirent
			binary.LittleEndian.PutUint64(blk[off:off+8], de.nid)
			// #nosec G115 -- a name starts inside its block, and a block is at most 1<<16 bytes
			binary.LittleEndian.PutUint16(blk[off+8:off+10], uint16(nameOff))
			blk[off+10] = de.fileType
			blk[off+11] = 0
			nameOff += copy(blk[nameOff:], de.name)
		}

		n, err := buf.Write(blk)

		totalWritten += n
		if err != nil {
			return totalWritten, err
		}

		// Pad to block boundary if there are more entries
		if i < len(allEnts) && blockUsed%w.blockSize != 0 {
			padSize := w.blockSize - (blockUsed % w.blockSize)
			if _, err := buf.Write(w.zeroBuf[:padSize]); err != nil {
				return totalWritten, err
			}

			totalWritten += padSize
		}
	}

	return totalWritten, nil
}

// writeDataBlocks writes data blocks for flat-plain entries directly to out.
//
//nolint:gocognit // one case per entry type, each data source copied and padded in place
func (w *erofsWriter) writeDataBlocks(out io.Writer) error {
	for _, e := range w.entries {
		ds := w.flatPlainDataSize(e)
		if ds == 0 {
			if err := w.writeTailBlock(out, e); err != nil {
				return err
			}

			continue
		}

		var n int

		switch e.mode & disk.StatTypeMask {
		case disk.StatTypeReg:
			expected := int64(ds)

			var (
				written int64
				err     error
			)

			// A source's file is opened here, where it is read, and seen as
			// what it is: the copy below knows an *os.File.
			data := e.data
			if s, ok := data.(*sourceFile); ok {
				f, ferr := s.file()
				if ferr != nil {
					return fmt.Errorf("write data for %s: %w", e.path, ferr)
				}

				data = f
			}

			limited := io.LimitReader(data, expected)
			// Use io.Copy for *os.File sources to enable copy_file_range.
			if _, ok := data.(*os.File); ok {
				written, err = io.Copy(out, limited)
			} else {
				written, err = io.CopyBuffer(onlyWriter{out}, limited, w.copyBuf)
			}

			if c, ok := e.data.(io.Closer); ok {
				_ = c.Close()
			}

			if err != nil {
				return fmt.Errorf("write data for %s: %w", e.path, err)
			}

			if written != expected {
				return fmt.Errorf("write data for %s: short read: got %d bytes, expected %d: %w",
					e.path, written, expected, io.ErrUnexpectedEOF)
			}

			n = int(written)
		case disk.StatTypeDir:
			written, err := w.writeDirents(out, e)
			if err != nil {
				return fmt.Errorf("write dirents for %s: %w", e.path, err)
			}

			n = written
		case disk.StatTypeSymlink:
			// Through the copy buffer: out takes bytes, and a string
			// handed to it as bytes is a copy of it made for the call.
			for left := e.symTarget; left != ""; {
				piece := w.copyBuf[:copy(w.copyBuf, left)]
				if _, err := out.Write(piece); err != nil {
					return fmt.Errorf("write symlink data for %s: %w", e.path, err)
				}

				left = left[len(piece):]
				n += len(piece)
			}
		}

		if n%w.blockSize != 0 {
			padSize := w.blockSize - (n % w.blockSize)
			if _, err := out.Write(w.zeroBuf[:padSize]); err != nil {
				return fmt.Errorf("write padding for %s: %w", e.path, err)
			}
		}

		if err := w.writeTailBlock(out, e); err != nil {
			return err
		}
	}

	return nil
}

// inlineEnd is the bytes of t, read from its image. The ends of a
// directory's files lie beside their inodes, one after another, and a read
// for each is a round trip to the source for each: so a read takes twice
// the one before it, tailReach at least and tailWindow at most, when t
// lies where that reaches, and serves the ends that follow. One that lies
// elsewhere is read alone: the order is the image's to choose, and a
// window for each would read many times the image.
func (w *erofsWriter) inlineEnd(t inlineTail) ([]byte, error) {
	last := int64(len(w.tailWin))

	from := t.at - w.tailAt
	if t.img != w.tailOf {
		from = -1
	}

	if from >= 0 && from+int64(t.n) <= last {
		return w.tailWin[from : from+int64(t.n)], nil
	}

	size := t.n
	if ahead := max(2*last, tailReach); from >= 0 && from-last < ahead {
		size = int(min(max(int64(size), ahead), tailWindow))
	}

	if w.tailWin == nil {
		w.tailWin = make([]byte, 0, tailWindow)
	}

	// A read that runs past the source's end comes back short, with the
	// end for an error: what counts is that t is in it.
	n, err := t.img.meta.ReadAt(w.tailWin[:size], t.at)
	w.tailOf, w.tailAt, w.tailWin = t.img, t.at, w.tailWin[:n]

	if n < t.n {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}

		return nil, err
	}

	return w.tailWin[:t.n], nil
}

// writeTailBlock writes a metadata-only file's copied tail as one block,
// where assignFlatPlainAddrs placed it.
func (w *erofsWriter) writeTailBlock(out io.Writer, e *erofsEntry) error {
	if e.tail.n == 0 {
		return nil
	}

	tail, err := w.inlineEnd(e.tail)
	if err != nil {
		return fmt.Errorf("read the inline end of %s where it was copied from: %w", e.path, err)
	}

	if _, err := out.Write(tail); err != nil {
		return fmt.Errorf("write tail for %s: %w", e.path, err)
	}

	if pad := w.blockSize - len(tail)%w.blockSize; pad < w.blockSize {
		if _, err := out.Write(w.zeroBuf[:pad]); err != nil {
			return fmt.Errorf("write padding for %s: %w", e.path, err)
		}
	}

	return nil
}

// flatPlainDataSize returns the size of the data e keeps in blocks of this
// image: a flat-plain entry's, or what a file with holes has beside them;
// 0 for any other.
func (w *erofsWriter) flatPlainDataSize(e *erofsEntry) int {
	if e.chunksOfData {
		return e.dataBytes
	}

	if e.layout != disk.LayoutFlatPlain {
		return 0
	}

	switch e.mode & disk.StatTypeMask {
	case disk.StatTypeReg:
		if e.size > 0 && e.data != nil {
			// e.size fits int64: File counts it in an int64, and add and copyFromImage bound it.
			// #nosec G115 -- and int is 64 bits wide on every supported platform
			return int(e.size)
		}
	case disk.StatTypeDir:
		return w.direntDataSize(e)
	case disk.StatTypeSymlink:
		return len(e.symTarget)
	}

	return 0
}
