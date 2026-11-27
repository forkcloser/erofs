package erofs_test

// What an image can make the reader hold, read or walk, each clause held by
// a measure: bytes allocated, bytes read, reads made. But for the chains of
// links, which the Writer makes, every image here is laid out by hand from
// the format's definitions, a few kilobytes that are not zero, since no
// writer emits what these clauses refuse; the rest of an image reads as
// zeros, as a sparse file's does.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/erofsspec"
)

// errLaidOffset is a read before the start of a laidImage.
var errLaidOffset = errors.New("laidImage: negative offset")

// laidImage is an image as a sparse file holds it: head is every byte that
// is not zero, and the rest up to size reads as zeros. It reports its
// length, as *os.File and *bytes.Reader do, and counts what is read from it.
type laidImage struct {
	head  []byte
	size  int64
	calls int64
	bytes int64
	// at counts the reads that start at each of the offsets it holds.
	at map[int64]int64
}

func (s *laidImage) Size() int64 { return s.size }

func (s *laidImage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errLaidOffset
	}

	if off >= s.size {
		return 0, io.EOF
	}

	n := len(p)
	if int64(n) > s.size-off {
		n = int(s.size - off)
	}

	clear(p[:n])

	if off < int64(len(s.head)) {
		copy(p[:n], s.head[off:])
	}

	s.calls++
	s.bytes += int64(n)

	if _, counted := s.at[off]; counted {
		s.at[off]++
	}

	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// whole is the image's bytes, zeros included.
func (s *laidImage) whole() []byte {
	out := make([]byte, s.size)
	copy(out, s.head)

	return out
}

// The format's values the laid images are made of.
const (
	laidFlatPlain  = 0
	laidFlatInline = 2
	laidChunked    = 4

	laidReg  = 0o100644
	laidDir  = 0o040755
	laidLink = 0o120777

	laidFtReg  = 1
	laidFtDir  = 2
	laidFtLink = 7

	// laidIndexes is the chunk format's bit for eight-byte indexes; without
	// it the map is a four-byte block address a chunk.
	laidIndexes = 0x20
)

// laidSuper writes a superblock without checksum at byte 1024 of head: the
// block size bits and the block count, the metadata starting at block 1
// with the root at nid 0, and the chunked-file feature.
func laidSuper(head []byte, blkBits uint8, blocks uint32) {
	sb := head[1024:]
	binary.LittleEndian.PutUint32(sb[0:], 0xe0f5e1e2)
	sb[12] = blkBits
	binary.LittleEndian.PutUint64(sb[16:], 1) // inos
	binary.LittleEndian.PutUint32(sb[36:], blocks)
	binary.LittleEndian.PutUint32(sb[40:], 1)   // meta_blkaddr
	binary.LittleEndian.PutUint32(sb[80:], 0x4) // INCOMPAT_CHUNKED_FILE
}

// laidCompact writes a 32-byte compact inode: its layout, mode, i_nb (the
// link count), size and i_u.
func laidCompact(dst []byte, layout uint8, mode, nb uint16, size, iu uint32) {
	binary.LittleEndian.PutUint16(dst[0:], uint16(layout)<<1)
	binary.LittleEndian.PutUint16(dst[4:], mode)
	binary.LittleEndian.PutUint16(dst[6:], nb)
	binary.LittleEndian.PutUint32(dst[8:], size)
	binary.LittleEndian.PutUint32(dst[16:], iu)
}

// laidExtended writes a 64-byte extended inode: its layout, mode, size, i_u
// and link count.
func laidExtended(dst []byte, layout uint8, mode uint16, size uint64, iu, nlink uint32) {
	binary.LittleEndian.PutUint16(dst[0:], uint16(layout)<<1|1)
	binary.LittleEndian.PutUint16(dst[4:], mode)
	binary.LittleEndian.PutUint64(dst[8:], size)
	binary.LittleEndian.PutUint32(dst[16:], iu)
	binary.LittleEndian.PutUint32(dst[44:], nlink)
}

type laidDirent struct {
	nid   uint64
	ftype uint8
	name  string
}

// laidDirBlock packs entries, already in name order, into one directory
// block: the dirents, then the names, the last ending where the block's
// zeros begin.
func laidDirBlock(ents []laidDirent) []byte {
	size := 0
	for _, e := range ents {
		size += 12 + len(e.name)
	}

	blk := make([]byte, size)
	nameOff := len(ents) * 12

	for i, e := range ents {
		binary.LittleEndian.PutUint64(blk[i*12:], e.nid)
		binary.LittleEndian.PutUint16(blk[i*12+8:], uint16(nameOff))
		blk[i*12+10] = e.ftype
		nameOff += copy(blk[nameOff:], e.name)
	}

	return blk
}

// allocatedSoFar is every byte the process has allocated.
func allocatedSoFar() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return m.TotalAlloc
}

// laidChunkedRoot is an image of 4 KiB blocks whose root directory is
// chunk-based, blocks chunks of one block each, its block map the zeros
// after its inode: every chunk is block 0, whose first kilobyte, before the
// superblock, is one block of 60 entries. The image is the superblock's
// block, then the inode and its map, rounded up to a block.
func laidChunkedRoot(blocks int64) *laidImage {
	const bs = 4096

	head := make([]byte, bs+64)

	ents := make([]laidDirent, 0, 60)
	for i := range 60 {
		ents = append(ents, laidDirent{nid: 2, ftype: laidFtReg, name: fmt.Sprintf("%02d", i)})
	}

	copy(head, laidDirBlock(ents))
	laidExtended(head[bs:], laidChunked, laidDir, uint64(blocks)*bs, 0, 2)

	size := (bs + 64 + blocks*4 + bs - 1) / bs * bs
	laidSuper(head, 12, uint32(size/bs))

	return &laidImage{head: head, size: size}
}

// laidRootOnDevice is an image of two 4 KiB blocks and its one extra device.
// The root directory is chunk-based with indexes, as many mapped chunks of
// one block each as asked, that name the device's blocks in turn; the device
// holds three blocks, of twenty entries each after the first one's own two,
// every one the empty regular file whose inode follows the root's map.
func laidRootOnDevice(mapped int) (img, device *laidImage) {
	const (
		bs     = 4096
		blocks = 3
	)

	// The root's 64 bytes, its map, then the file's inode on the next slot.
	fileAt := (bs + 64 + mapped*8 + 31) / 32 * 32
	head := make([]byte, fileAt+32)

	laidSuper(head, 12, 2)

	sb := head[1024:]
	binary.LittleEndian.PutUint32(sb[80:], 0x4|0x8) // INCOMPAT_CHUNKED_FILE, INCOMPAT_DEVICE_TABLE
	binary.LittleEndian.PutUint16(sb[86:], 1)       // extra_devices
	binary.LittleEndian.PutUint16(sb[88:], 16)      // devt_slotoff, in slots of 128 bytes

	binary.LittleEndian.PutUint32(head[16*128+64:], uint32(blocks)) // the slot's block count

	laidExtended(head[bs:], laidChunked, laidDir, uint64(mapped)*bs, laidIndexes, 2)

	for i := range mapped {
		index := head[bs+64+i*8:]
		binary.LittleEndian.PutUint16(index[2:], 1)                // device_id
		binary.LittleEndian.PutUint32(index[4:], uint32(i%blocks)) // startblk_lo
	}

	laidCompact(head[fileAt:], laidFlatPlain, laidReg, 1, 0, 0)

	held := make([]byte, blocks*bs)

	for b := range blocks {
		ents := make([]laidDirent, 0, 22)
		if b == 0 {
			ents = append(ents,
				laidDirent{nid: 0, ftype: laidFtDir, name: "."},
				laidDirent{nid: 0, ftype: laidFtDir, name: ".."})
		}

		for i := range 20 {
			ents = append(ents, laidDirent{
				nid: uint64((fileAt - bs) / 32), ftype: laidFtReg, name: fmt.Sprintf("%02d-%02d", b, i),
			})
		}

		copy(held[b*bs:], laidDirBlock(ents))
	}

	return &laidImage{head: head, size: 2 * bs}, &laidImage{head: held, size: int64(len(held))}
}

// TestContractReaderDirectoryOnADevice: a directory's entries may be an
// extra device's bytes, so what a directory is held to is what the image and
// its devices hold together: one of three blocks on a device, in an image of
// two, is listed and looked up in, as the kernel's readdir maps a
// chunk-based directory's chunks to the device they name. It was refused for
// declaring more bytes than the image. One that declares more than the image
// and the device hold is ErrCorrupt.
func TestContractReaderDirectoryOnADevice(t *testing.T) {
	t.Parallel()

	img, device := laidRootOnDevice(3)

	fsys, err := erofs.Open(img, erofs.WithExtraDevices(device))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil || len(entries) != 3*20 {
		t.Fatalf("a directory of three blocks on a device: %d entries, %v; want %d", len(entries), err, 3*20)
	}

	if info, statErr := fs.Stat(fsys, "02-19"); statErr != nil || !info.Mode().IsRegular() {
		t.Errorf("the last name of the third block: %v, %v; want a regular file", info, statErr)
	}

	// Six blocks mapped, where the image holds two and the device three.
	img, device = laidRootOnDevice(6)

	if fsys, err = erofs.Open(img, erofs.WithExtraDevices(device)); err != nil {
		t.Fatal(err)
	}

	if _, err = fs.ReadDir(fsys, "."); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("a directory of six blocks over an image of two and a device of three: %v; want ErrCorrupt", err)
	}
}

// unmeasured is a device whose reader reports no length.
type unmeasured struct{ held io.ReaderAt }

func (u unmeasured) ReadAt(p []byte, off int64) (int, error) { return u.held.ReadAt(p, off) }

// TestContractReaderDirectoryOnADeviceOfNoLength: a device given through a
// reader that reports no length gives no measure and adds nothing to what a
// directory is held to: the root of three blocks on such a device, in an
// image of two, is ErrCorrupt, and is read once the same device is given
// with its length.
func TestContractReaderDirectoryOnADeviceOfNoLength(t *testing.T) {
	t.Parallel()

	img, device := laidRootOnDevice(3)

	fsys, err := erofs.Open(img, erofs.WithExtraDevices(unmeasured{device}))
	if err != nil {
		t.Fatal(err)
	}

	if _, err = fs.Stat(fsys, "."); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("a directory of three blocks on a device of no length, in an image of two: %v; want ErrCorrupt", err)
	}

	if fsys, err = erofs.Open(img, erofs.WithExtraDevices(device)); err != nil {
		t.Fatal(err)
	}

	if _, err = fs.Stat(fsys, "."); err != nil {
		t.Errorf("the same directory, the device given with its length: %v", err)
	}
}

// TestContractReaderDirectoryBoundIsNotTheImagesWord: opened with none of
// its devices, an image is all there is, and what its device table says of
// a device adds nothing to what a directory is held to: a root of three
// blocks in an image of two is ErrCorrupt whether the slot declares no
// blocks past the image's end, four billion of them, or the image's own two
// over again.
func TestContractReaderDirectoryBoundIsNotTheImagesWord(t *testing.T) {
	t.Parallel()

	for _, slot := range []struct {
		name           string
		blocks, mapped uint32
	}{
		{"no blocks, mapped past the end", 0, 100},
		{"every block there can be", math.MaxUint32, 100},
		{"the image's own blocks", 2, 0},
	} {
		img, _ := laidRootOnDevice(3)

		binary.LittleEndian.PutUint32(img.head[16*128+64:], slot.blocks)
		binary.LittleEndian.PutUint32(img.head[16*128+68:], slot.mapped) // mapped_blkaddr

		fsys, err := erofs.Open(img)
		if err != nil {
			t.Fatalf("%s: Open: %v", slot.name, err)
		}

		if _, err = fs.Stat(fsys, "."); !errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("%s: a directory of three blocks in an image of two: %v; want ErrCorrupt", slot.name, err)
		}
	}
}

// TestContractReaderDirectoryIsHeldToItsImage: a directory's entries are its
// image's own bytes, so a directory declaring more bytes than the image's
// extent is ErrCorrupt with its inode, at Stat and at ReadDir, before
// anything is allocated for an entry; one of exactly the extent is listed.
// The extent is the reader's length, or the superblock's block count when
// the reader reports none. Without the bound a chunk map naming one block
// for every chunk is a block of entries more for every four bytes of image:
// the 24 KiB image here declares 245760 entries, 54 MiB allocated by one
// ReadDir.
//
// The reference lists the directory one block over: the bound is this
// reader's, where the kernel reads the blocks a map names however often it
// names one.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractReaderDirectoryIsHeldToItsImage(t *testing.T) {
	const budget = 64 << 10

	for _, tc := range []struct {
		name string
		open func(*laidImage) io.ReaderAt
	}{
		{"a reader with a length", func(s *laidImage) io.ReaderAt { return s }},
		{"a reader without one", func(s *laidImage) io.ReaderAt { return readAtOnly{s} }},
	} {
		src := laidChunkedRoot(4096)

		fsys, err := erofs.Open(tc.open(src))
		if err != nil {
			t.Fatalf("%s: Open: %v", tc.name, err)
		}

		before := allocatedSoFar()

		entries, err := fs.ReadDir(fsys, ".")

		allocated := allocatedSoFar() - before

		if !errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("%s: ReadDir of a %d byte directory in a %d byte image: %d entries, %v; want ErrCorrupt",
				tc.name, int64(4096)*4096, src.size, len(entries), err)
		}

		if allocated > budget {
			t.Errorf("%s: ReadDir allocated %d bytes; want under %d", tc.name, allocated, budget)
		}

		if _, err := fs.Stat(fsys, "."); !errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("%s: Stat of the directory: %v; want ErrCorrupt", tc.name, err)
		}
	}

	// Two blocks in an image of two blocks: the extent itself.
	src := laidChunkedRoot(2)

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil || len(entries) != 2*60 {
		t.Errorf(
			"a directory of the image's extent (%d bytes): %d entries, %v; want %d",
			src.size,
			len(entries),
			err,
			2*60,
		)
	}

	// Three blocks in an image of two: one block over.
	src = laidChunkedRoot(3)

	fsys, err = erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = fs.ReadDir(fsys, "."); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("a directory one block over its image: %v; want ErrCorrupt", err)
	}

	ref, err := erofsspec.Parse(src.whole())
	if err != nil {
		t.Fatal(err)
	}

	root, err := ref.Inode(ref.Super.RootNid)
	if err != nil {
		t.Fatal(err)
	}

	if listed, err := ref.Dirents(root); err != nil || len(listed) != 3*60 {
		t.Errorf("the reference on the directory one block over: %d entries, %v; want %d", len(listed), err, 3*60)
	}
}

// laidChunkedFile is an image of 4 KiB blocks whose root holds one
// chunk-based regular file "f" of chunks chunks, one block each. Its map
// starts at byte laidMapOff of the image, right after its inode, and is the
// zeros there: every chunk is block 0. format is the chunk format: 0 for a
// block map, four bytes a chunk, laidIndexes for indexes, eight. The image
// declares the map, as a sparse file would hold it.
func laidChunkedFile(chunks int64, format uint32) *laidImage {
	const bs = 4096

	head := make([]byte, laidMapOff)

	// Root at nid 0: an inline directory of ".", ".." and "f".
	root := laidDirBlock([]laidDirent{{0, laidFtDir, "."}, {0, laidFtDir, ".."}, {3, laidFtReg, "f"}})
	laidCompact(head[bs:], laidFlatInline, laidDir, 2, uint32(len(root)), 0)
	copy(head[bs+32:], root)

	// "f" at nid 3: an extended inode, its map right after it.
	laidExtended(head[bs+96:], laidChunked, laidReg, uint64(chunks)*bs, format, 1)

	unit := int64(4)
	if format&laidIndexes != 0 {
		unit = 8
	}

	size := (laidMapOff + chunks*unit + bs - 1) / bs * bs
	laidSuper(head, 12, uint32(size/bs))

	return &laidImage{head: head, size: size}
}

// laidMapOff is where the chunk map of laidChunkedFile's "f" starts: the
// superblock's block, the root and its entries, then the file's inode.
const laidMapOff = 4096 + 96 + 64

// TestContractReaderStatHoldsNothingOfAChunkMap: Stat of a chunk-based file
// judges every chunk of its map and holds none of it: what it allocates
// does not grow with the map. DataRange, asked, then builds the ranges, one
// a chunk here since no two follow each other, and allocates them once, at
// their size. The map is a megabyte of zeros; a Stat that reads it whole
// and builds its ranges to judge them allocates 35 MiB for it, and DataRange
// as much again.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractReaderStatHoldsNothingOfAChunkMap(t *testing.T) {
	const (
		chunks = 1 << 18 // a block map of 1 MiB
		budget = 128 << 10
	)

	src := laidChunkedFile(chunks, 0)

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.bytes = 0
	before := allocatedSoFar()

	info, err := fs.Stat(fsys, "f")
	if err != nil {
		t.Fatal(err)
	}

	if allocated := allocatedSoFar() - before; allocated > budget {
		t.Errorf(
			"Stat of a file with a %d byte chunk map allocated %d bytes; want under %d",
			chunks*4,
			allocated,
			budget,
		)
	}

	// Every chunk was judged: the whole map was read.
	if src.bytes < chunks*4 {
		t.Errorf("Stat read %d bytes of a %d byte chunk map", src.bytes, chunks*4)
	}

	before = allocatedSoFar()

	ranges := info.(interface{ DataRange() []erofs.DataRange }).DataRange()

	// A DataRange is 24 bytes.
	if allocated := allocatedSoFar() - before; allocated > chunks*24+budget {
		t.Errorf("DataRange allocated %d bytes for %d ranges of 24 bytes", allocated, len(ranges))
	}

	if len(ranges) != chunks {
		t.Fatalf("DataRange: %d ranges; want one for each of the %d chunks", len(ranges), chunks)
	}

	for i, r := range ranges {
		if r != (erofs.DataRange{Offset: 0, Size: 4096}) {
			t.Fatalf("DataRange: range %d is %+v; want block 0", i, r)
		}
	}

	// A chunk past the image is still refused at Stat, wherever it is in
	// the map: here the last one, in the map's last piece.
	bad := &laidImage{head: slices.Concat(src.head, make([]byte, chunks*4)), size: src.size}
	binary.LittleEndian.PutUint32(bad.head[laidMapOff+(chunks-1)*4:], uint32(src.size/4096))

	fsys, err = erofs.Open(bad)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = fs.Stat(fsys, "f"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("Stat of a file whose last chunk is past the image: %v; want ErrCorrupt", err)
	}
}

// TestContractReaderAcceptsEOFOnAFullChunkMapRead: a read of a chunk map
// that ends exactly at the image's last byte may come back as (len(p),
// io.EOF), as a block read may, and the file is served. The map of 984
// chunks here ends with the image's second block.
func TestContractReaderAcceptsEOFOnAFullChunkMapRead(t *testing.T) {
	t.Parallel()

	const chunks = (2*4096 - laidMapOff) / 4

	var atEnd atomic.Int64

	fsys, err := erofs.Open(eofReaderAt{data: laidChunkedFile(chunks, 0).whole(), atEnd: &atEnd})
	if err != nil {
		t.Fatal(err)
	}

	atEnd.Store(0)

	info, err := fs.Stat(fsys, "f")
	if err != nil {
		t.Fatalf("Stat of a file whose chunk map ends with the image: %v", err)
	}

	if atEnd.Load() == 0 {
		t.Fatal("no read ended at the image's last byte; the case was not met")
	}

	if ranges := info.(interface{ DataRange() []erofs.DataRange }).DataRange(); len(ranges) != chunks {
		t.Errorf("DataRange: %d ranges; want %d", len(ranges), chunks)
	}
}

// TestContractReaderStatAllocatesNothingForAnAbsentChunkMap: behind a
// reader that reports no length the image's extent is the superblock's
// word, and a chunk map the superblock's extent holds but the reader does
// not is refused at Stat without a buffer of its size: the source here ends
// after the inode's block and declares a map of 64 MiB, the most a file may
// have.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractReaderStatAllocatesNothingForAnAbsentChunkMap(t *testing.T) {
	const (
		chunks = 16 << 20 // a block map of 64 MiB
		budget = 128 << 10
	)

	src := laidChunkedFile(chunks, 0)
	src.size = 2 * 4096

	fsys, err := erofs.Open(readAtOnly{src})
	if err != nil {
		t.Fatal(err)
	}

	before := allocatedSoFar()

	if _, err := fs.Stat(fsys, "f"); err == nil {
		t.Error("Stat served a file whose chunk map is not in the source")
	}

	if allocated := allocatedSoFar() - before; allocated > budget {
		t.Errorf("Stat allocated %d bytes for a chunk map the source does not hold; want under %d", allocated, budget)
	}
}

// TestContractReaderReadTakesABlockOfTheMapNotTheMap: a Read of a file with
// chunk indexes reads the block of its map that the read is in, and no more
// of the map: one byte of a file whose megabyte of indexes all name block 0
// costs a block of the index, not the index. A file that is one run is read
// a run at a time, however long its index: sixteen blocks are one read of
// the image once that block of the map is held.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractReaderReadTakesABlockOfTheMapNotTheMap(t *testing.T) {
	const (
		chunks = 1 << 17 // 1 MiB of indexes
		budget = 128 << 10
	)

	src := laidChunkedFile(chunks, laidIndexes)

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	f, err := fsys.Open("f")
	if err != nil {
		t.Fatal(err)
	}

	src.bytes = 0
	before := allocatedSoFar()

	var one [1]byte
	if _, err = io.ReadFull(f, one[:]); err != nil {
		t.Fatal(err)
	}

	if allocated := allocatedSoFar() - before; allocated > budget || src.bytes > budget {
		t.Errorf(
			"Read of one byte of a file with %d bytes of indexes allocated %d bytes and read %d; want each under %d",
			chunks*8,
			allocated,
			src.bytes,
			budget,
		)
	}

	_ = f.Close()

	// One run of 16384 chunks from block 34, the first after its index: the
	// index is two pieces, and the file's first bytes are in the image's
	// head.
	const (
		run   = 16384
		start = 34
		mark  = "one run"
	)

	src = laidChunkedFile(run, laidIndexes)
	src.head = append(src.head, make([]byte, start*4096+len(mark)-laidMapOff)...)

	for i := range run {
		binary.LittleEndian.PutUint32(src.head[laidMapOff+i*8+4:], uint32(start+i))
	}

	copy(src.head[start*4096:], mark)

	src.size = (start + run) * 4096
	laidSuper(src.head, 12, start+run)

	if fsys, err = erofs.Open(src); err != nil {
		t.Fatal(err)
	}

	if f, err = fsys.Open("f"); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 16*4096)
	if _, err = io.ReadFull(f, buf); err != nil || string(buf[:len(mark)]) != mark {
		t.Fatalf("the first read of the run: %q, %v; want %q", buf[:len(mark)], err, mark)
	}

	src.calls = 0

	if _, err = io.ReadFull(f, buf); err != nil {
		t.Fatal(err)
	}

	if src.calls != 1 {
		t.Errorf("a Read of 16 blocks of a file that is one run took %d reads; want 1", src.calls)
	}

	_ = f.Close()
}

// laidLinkLoop is an image of blocks of 1<<blkBits bytes whose root holds a
// directory "a", which lists itself as "a", and a symlink "l" to target,
// listed in the root and in "a": a target through "a" that ends in "l"
// comes back to the link at every hop. Each inode starts a block: the root
// in block 1, "a" in block 2, the link in block 3, its target in block 4.
func laidLinkLoop(blkBits uint8, target string) *laidImage {
	bs := 1 << blkBits
	slots := uint64(bs / 32)

	head := make([]byte, 4*bs+len(target))

	ents := func(self uint64) []byte {
		return laidDirBlock([]laidDirent{
			{self, laidFtDir, "."}, {0, laidFtDir, ".."}, {slots, laidFtDir, "a"}, {2 * slots, laidFtLink, "l"},
		})
	}

	root, a := ents(0), ents(slots)

	laidCompact(head[bs:], laidFlatInline, laidDir, 3, uint32(len(root)), 0)
	copy(head[bs+32:], root)

	laidCompact(head[2*bs:], laidFlatInline, laidDir, 3, uint32(len(a)), 0)
	copy(head[2*bs+32:], a)

	laidCompact(head[3*bs:], laidFlatPlain, laidLink, 1, uint32(len(target)), 4)
	copy(head[4*bs:], target)

	laidSuper(head, blkBits, 5)

	return &laidImage{head: head, size: int64(5 * bs)}
}

// TestContractReaderResolutionReadsInodesNotTheirBlocks: an inode is read
// as the 64 bytes it is at most, never with the rest of its block, so what
// a resolution reads does not grow with the block size. One Open at the
// resolution's limit, 255 hops of 2047 components each, over an image of
// 64 KiB blocks where every inode starts one, reads under 128 bytes a
// component, an inode and its directory's four entries, and under two pages
// a hop for the link and its target: 59 MiB in all, where reading each
// inode's block is 32 GiB.
func TestContractReaderResolutionReadsInodesNotTheirBlocks(t *testing.T) {
	t.Parallel()

	const (
		hops       = 255
		components = 2047
		budget     = hops*components*128 + hops*2*4096
	)

	src := laidLinkLoop(16, strings.Repeat("/a", components-1)+"/l")

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.bytes = 0

	if _, err = fsys.Open("l"); !errors.Is(err, erofs.ErrLoop) {
		t.Fatalf("Open of a link that leads to itself: %v; want ErrLoop", err)
	}

	if src.bytes > budget {
		t.Errorf("one Open read %d bytes from a %d byte image; want under %d", src.bytes, src.size, budget)
	}
}

// TestContractReaderReadFileLimit: io/fs.ReadFile refuses a file over
// 128 MiB as ErrInvalid alone, before it allocates anything for it, and
// takes a file of exactly 128 MiB. The file here is flat, its first block
// the first past the image: once the limit has let it through, the read
// fails as ErrCorrupt, and nothing is written to its 128 MiB.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractReaderReadFileLimit(t *testing.T) {
	const limit = 128 << 20

	open := func(size uint64) fs.FS {
		head := make([]byte, laidMapOff)

		root := laidDirBlock([]laidDirent{{0, laidFtDir, "."}, {0, laidFtDir, ".."}, {3, laidFtReg, "f"}})
		laidCompact(head[4096:], laidFlatInline, laidDir, 2, uint32(len(root)), 0)
		copy(head[4096+32:], root)

		// "f" at nid 3: its data from block 2 of an image of two.
		laidExtended(head[4096+96:], laidFlatPlain, laidReg, size, 2, 1)
		laidSuper(head, 12, 2)

		fsys, err := erofs.Open(&laidImage{head: head, size: 2 * 4096})
		if err != nil {
			t.Fatal(err)
		}

		return fsys
	}

	over, at := open(limit+1), open(limit)

	before := allocatedSoFar()

	_, err := fs.ReadFile(over, "f")

	allocated := allocatedSoFar() - before

	if !errors.Is(err, erofs.ErrInvalid) || errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("ReadFile of a file of %d bytes: %v; want ErrInvalid alone", limit+1, err)
	}

	if allocated > 64<<10 {
		t.Errorf("ReadFile allocated %d bytes for a file over its limit", allocated)
	}

	if _, err = fs.ReadFile(at, "f"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf(
			"ReadFile of a file of %d bytes whose data is past the image: %v; want ErrCorrupt, the limit letting it through",
			limit,
			err,
		)
	}
}

// TestContractReaderResolveFollows255Links: a resolution follows 255
// symlinks and no more. A chain of 255 links to a file opens the file; a
// chain of 256 is ErrLoop.
func TestContractReaderResolveFollows255Links(t *testing.T) {
	t.Parallel()

	chain := func(links int) fs.FS {
		img := buildImage(t, func(w *erofs.Writer) {
			writeFile(t, w, "/f", []byte("the end"))

			for i := range links {
				target := "f"
				if i < links-1 {
					target = fmt.Sprintf("l%d", i+1)
				}

				if err := w.Symlink(target, fmt.Sprintf("/l%d", i)); err != nil {
					t.Fatal(err)
				}
			}
		})

		fsys, err := erofs.Open(bytes.NewReader(img))
		if err != nil {
			t.Fatal(err)
		}

		return fsys
	}

	if data, err := fs.ReadFile(chain(255), "l0"); err != nil || string(data) != "the end" {
		t.Errorf("a file behind 255 links: %q, %v; want it read", data, err)
	}

	if _, err := fs.ReadFile(chain(256), "l0"); !errors.Is(err, erofs.ErrLoop) {
		t.Errorf("a file behind 256 links: %v; want ErrLoop", err)
	}
}

// TestContractReaderResolveHoldsEveryHopToThePathLimit: the path a symlink
// composes is held to 4096 bytes at every hop, and a longer one is
// ErrInvalid, which is not ErrLoop. A relative target of 1001 bytes that
// comes back to its link through a directory listing itself adds 1000
// bytes to the path at each hop, so the fifth path is refused: the
// resolution is four walks, of 501, 1001, 1501 and 2001 names, not the
// 255 hops of an ever longer path the hop count alone would allow.
func TestContractReaderResolveHoldsEveryHopToThePathLimit(t *testing.T) {
	t.Parallel()

	// Two reads a name, its directory's inode and entries, and at each hop
	// the link's inode and target.
	const budget = 2*(1+501+1001+1501+2001) + 2*5

	src := laidLinkLoop(12, strings.Repeat("a/", 500)+"l")

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.calls = 0

	_, err = fsys.Open("l")
	if !errors.Is(err, erofs.ErrInvalid) || errors.Is(err, erofs.ErrLoop) {
		t.Errorf("Open of a link whose path grows at each hop: %v; want ErrInvalid for the path, not ErrLoop", err)
	}

	if src.calls > budget {
		t.Errorf("Open made %d reads before refusing the path; want at most %d", src.calls, budget)
	}
}

// TestContractReaderResolveLooksUpAtMost255Paths: one resolution looks up
// at most 255 times 2048 names in all, 255 hops of the longest path there
// is, the walk of the name it was given included. A name of 2048 components
// ending in a link back to 2047 of them, the longest target there is,
// reaches that budget one path before it has followed its 256th link, and
// is ErrLoop there. Each name looked up is one read of its directory's
// inode, the root's or "a"'s, which is what is counted.
func TestContractReaderResolveLooksUpAtMost255Paths(t *testing.T) {
	t.Parallel()

	const budget = 255 * 2048

	name := strings.Repeat("a/", 2047) + "l"
	src := laidLinkLoop(12, "/"+strings.Repeat("a/", 2046)+"l")

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	// The root's inode and "a"'s, each at the start of its block.
	src.at = map[int64]int64{4096: 0, 2 * 4096: 0}

	if _, err = fsys.Open(name); !errors.Is(err, erofs.ErrLoop) {
		t.Fatalf("Open of a link that leads to itself: %v; want ErrLoop", err)
	}

	if lookups := src.at[4096] + src.at[2*4096]; lookups > budget {
		t.Errorf("one Open looked up %d names; want at most %d", lookups, budget)
	} else if lookups < budget-2048 {
		t.Errorf(
			"one Open looked up %d names: the walk ended a path before its budget of %d, by another bound",
			lookups,
			budget,
		)
	}
}

// TestContractReaderRefusesAnImageWithoutAnExtent: an image whose extent
// cannot be known is ErrCorrupt at Open. A superblock's block count is the
// image's own statement of its size, and zero is not the size of an image
// that holds a root directory: behind a reader that reports no length,
// nothing such an image declares can be held to anything. Through a reader
// that has a length the same image opens, and is held to that length: its
// directory of the reader's two blocks is listed, and one of three is
// ErrCorrupt.
func TestContractReaderRefusesAnImageWithoutAnExtent(t *testing.T) {
	t.Parallel()

	noBlocks := func(blocks int64) *laidImage {
		src := laidChunkedRoot(blocks)
		binary.LittleEndian.PutUint32(src.head[1024+36:], 0)

		return src
	}

	if _, err := erofs.Open(readAtOnly{noBlocks(2)}); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("Open of an image declaring no blocks, through a reader without a length: %v; want ErrCorrupt", err)
	}

	fsys, err := erofs.Open(noBlocks(2))
	if err != nil {
		t.Fatalf("Open of an image declaring no blocks, through a reader with a length: %v", err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil || len(entries) != 2*60 {
		t.Errorf("a directory of the reader's length: %d entries, %v; want %d", len(entries), err, 2*60)
	}

	if fsys, err = erofs.Open(noBlocks(3)); err != nil {
		t.Fatal(err)
	}

	if _, err = fs.ReadDir(fsys, "."); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("a directory one block over the reader's length: %v; want ErrCorrupt", err)
	}
}
