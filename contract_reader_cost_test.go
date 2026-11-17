package erofs_test

// What an image can make the reader hold, read or walk, each clause held by
// a measure: bytes allocated, bytes read, reads made. Every image
// here is laid out by hand from the format's definitions, a few kilobytes
// that are not zero, since no writer emits what these clauses refuse; the
// rest of an image reads as zeros, as a sparse file's does.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// errLaidOffset is a read before the start of a laidImage.
var errLaidOffset = errors.New("laidImage: negative offset")

// laidImage is an image as a sparse file holds it: head is every byte that
// is not zero, and the rest up to size reads as zeros. It reports its
// length, as *os.File and *bytes.Reader do.
type laidImage struct {
	head []byte
	size int64
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
	laidChunked = 4

	laidDir = 0o040755

	laidFtReg = 1
)

// laidSuper writes a superblock without checksum at byte 1024 of head: block
// size bits, block count, the metadata's and the shared xattrs' first block,
// the root's nid, and the chunked-file feature.
func laidSuper(head []byte, blkBits uint8, blocks, metaBlk uint32, rootNid uint16) {
	sb := head[1024:]
	binary.LittleEndian.PutUint32(sb[0:], 0xe0f5e1e2)
	sb[12] = blkBits
	binary.LittleEndian.PutUint16(sb[14:], rootNid)
	binary.LittleEndian.PutUint64(sb[16:], 1) // inos
	binary.LittleEndian.PutUint32(sb[36:], blocks)
	binary.LittleEndian.PutUint32(sb[40:], metaBlk)
	binary.LittleEndian.PutUint32(sb[80:], 0x4) // INCOMPAT_CHUNKED_FILE
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
	laidSuper(head, 12, uint32(size/bs), 1, 0)

	return &laidImage{head: head, size: size}
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
