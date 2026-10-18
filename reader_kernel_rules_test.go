// Copyright 2026 Forkcloser. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/disk"
)

// The reader follows the kernel's rules where an image departs from what
// mkfs.erofs writes: the superblock checksum is verified before the feature
// bits are read, an i_format bit the kernel does not define is a format this
// reader lacks, a compact flat-plain file at the null address reads as zeros, and a
// FIFO or a socket has no device number whatever its i_u slot holds.

// buildImage writes a small tree and returns the image's bytes.
func buildImage(t *testing.T, build func(w *erofs.Writer)) []byte {
	t.Helper()

	var buf testBuffer

	w := erofs.Create(&buf)
	build(w)

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// inodeOffset is the byte offset of name's inode in img; i_u, the start
// block or the device number, is sixteen bytes in, in both inode sizes.
func inodeOffset(t *testing.T, img []byte, name string) int {
	t.Helper()

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	st, err := fs.Lstat(fsys, name)
	if err != nil {
		t.Fatal(err)
	}

	ino, ok := st.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("Sys is %T", st.Sys())
	}

	var sb disk.SuperBlock

	sb.Unmarshal(img[disk.SuperBlockOffset : disk.SuperBlockOffset+disk.SizeSuperBlock])

	return int(sb.MetaBlkAddr)<<sb.BlkSizeBits + int(ino.Ino)*disk.SizeInodeCompact
}

// stampChecksum sets the checksum feature on img and writes the checksum the
// kernel computes: CRC-32C with seed 0x5045B54A, no inversion, from the byte
// after the checksum field to the end of the first block less the
// superblock offset.
func stampChecksum(img []byte) {
	sb := img[disk.SuperBlockOffset:]
	binary.LittleEndian.PutUint32(sb[8:12], binary.LittleEndian.Uint32(sb[8:12])|disk.FeatureCompatSBChksum)

	length := 1 << sb[12]
	if length > disk.SuperBlockOffset {
		length -= disk.SuperBlockOffset
	}

	crc := ^crc32.Update(^uint32(0x5045B54A), crc32.MakeTable(crc32.Castagnoli), sb[8:length])
	binary.LittleEndian.PutUint32(sb[4:8], crc)
}

func TestReaderSuperblockChecksum(t *testing.T) {
	t.Parallel()

	img := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/a", bytes.Repeat([]byte("a"), 3000)) })
	stampChecksum(img)

	if _, err := erofs.Open(bytes.NewReader(img)); err != nil {
		t.Fatalf("a stamped image: %v", err)
	}

	bad := bytes.Clone(img)
	bad[disk.SuperBlockOffset+4]++

	if _, err := erofs.Open(bytes.NewReader(bad)); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("a wrong checksum: %v; want ErrCorrupt", err)
	}

	// A byte of the checksummed area, past the superblock's own fields.
	bad = bytes.Clone(img)
	bad[disk.SuperBlockOffset+disk.SizeSuperBlock+10] ^= 0x40

	if _, err := erofs.Open(bytes.NewReader(bad)); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("a flipped block byte: %v; want ErrCorrupt", err)
	}

	// Damage over feature_incompat (bytes 80 to 83 of the superblock) is
	// corruption, not a feature this reader lacks: the checksum is judged
	// first, as the kernel does.
	bad = bytes.Clone(img)
	for i := range 4 {
		bad[disk.SuperBlockOffset+80+i] = 0xFF
	}

	if _, err := erofs.Open(bytes.NewReader(bad)); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("damaged feature words under a checksum: %v; want ErrCorrupt", err)
	}

	// A reader that fails over the checksummed span reports its own error,
	// not a corrupt image.
	failing := failingAt{bytes.NewReader(img), disk.SuperBlockOffset + 8, errDisk}
	if _, err := erofs.Open(failing); !errors.Is(err, errDisk) {
		t.Fatalf("a failing reader: %v; want its own error", err)
	}

	// Without the feature bit nothing is checked, whatever the field holds.
	plain := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/a", []byte("a")) })
	plain[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum
	plain[disk.SuperBlockOffset+4] = 0x5A

	if _, err := erofs.Open(bytes.NewReader(plain)); err != nil {
		t.Fatalf("an unchecked image: %v", err)
	}
}

// TestReaderIshareXattrPrefixID: under the ishare-xattrs compatible
// feature, an ishare prefix id at or past the long prefix count is a
// corrupt superblock, refused by Open as the kernel refuses the mount. The
// Writer emits no prefix table, so any id is past it; without the feature
// the field is not read.
func TestReaderIshareXattrPrefixID(t *testing.T) {
	t.Parallel()

	img := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/a", []byte("a")) })

	for _, id := range []byte{0, 3} {
		bad := bytes.Clone(img)
		bad[disk.SuperBlockOffset+8] |= disk.FeatureCompatIshareXattrs
		bad[disk.SuperBlockOffset+105] = id
		stampChecksum(bad)

		if _, err := erofs.Open(bytes.NewReader(bad)); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("ishare prefix id %d of 0: %v; want ErrCorrupt", id, err)
		}
	}

	plain := bytes.Clone(img)
	plain[disk.SuperBlockOffset+105] = 3
	stampChecksum(plain)

	if _, err := erofs.Open(bytes.NewReader(plain)); err != nil {
		t.Fatalf("an ishare prefix id without the feature: %v", err)
	}

	// An mkfs.erofs image with the feature set and the checksum feature
	// cleared, so that nothing but the id refuses it.
	mkfs, err := os.ReadFile("internal/corpus/testdata/small-b512.erofs")
	if err != nil {
		t.Fatal(err)
	}

	mkfs[disk.SuperBlockOffset+8] = 0xA8

	if _, err := erofs.Open(bytes.NewReader(mkfs)); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("an mkfs image with the feature and no prefixes: %v; want ErrCorrupt", err)
	}
}

// errDisk is a failing reader's own error.
var errDisk = errors.New("disk")

// failingAt is a reader whose ReadAt fails with err at or past off.
type failingAt struct {
	r   io.ReaderAt
	off int64
	err error
}

func (f failingAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.off {
		return 0, f.err
	}

	return f.r.ReadAt(p, off)
}

// withBit returns img with bit set in the i_format of the inode at.
func withBit(img []byte, at int, bit uint16) []byte {
	bad := bytes.Clone(img)
	binary.LittleEndian.PutUint16(bad[at:], binary.LittleEndian.Uint16(bad[at:])|bit)

	return bad
}

// compactHole rewrites the extended inode at as a compact regular file of
// size bytes, flat-plain at the null address, so it reads as zeros without
// any data behind it. The Writer emits extended inodes only.
func compactHole(img []byte, at int, size uint32) {
	ino := img[at : at+disk.SizeInodeCompact]
	clear(ino)
	binary.LittleEndian.PutUint16(ino[0:], disk.LayoutFlatPlain<<1)
	binary.LittleEndian.PutUint16(ino[4:], disk.StatTypeReg|0o644)
	binary.LittleEndian.PutUint16(ino[6:], 1)
	binary.LittleEndian.PutUint32(ino[8:], size)
	binary.LittleEndian.PutUint32(ino[16:], disk.NullAddr)
}

// TestReaderInodeFormatBits: a bit the kernel does not define is
// ErrNotImplemented on any inode. Bit 4 is dot-omitted on a directory,
// which lists as before; nlink-1 on a compact non-directory inode, refused
// as the 48-bit layout is; ignored on an extended non-directory inode, as
// the kernel ignores it.
func TestReaderInodeFormatBits(t *testing.T) {
	t.Parallel()

	content := bytes.Repeat([]byte("a"), 3000)
	img := buildImage(t, func(w *erofs.Writer) {
		writeFile(t, w, "/a", []byte("a"))
		writeFile(t, w, "/big", content)

		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		writeFile(t, w, "/d/x", []byte("x"))
	})

	compactAt := inodeOffset(t, img, "a")
	compactHole(img, compactAt, 1)

	plain, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if got, rerr := fs.ReadFile(plain, "a"); rerr != nil || !bytes.Equal(got, []byte{0}) {
		t.Fatalf("the compact hole file: %q, %v; want one zero", got, rerr)
	}

	for _, name := range []string{"a", "big", "d"} {
		at := inodeOffset(t, img, name)

		for _, bit := range []uint16{0x0020, 0x8000} {
			undefined, oerr := erofs.Open(bytes.NewReader(withBit(img, at, bit)))
			if oerr != nil {
				t.Fatal(oerr)
			}

			if _, lerr := fs.Lstat(undefined, name); !errors.Is(lerr, erofs.ErrNotImplemented) {
				t.Fatalf("%s: i_format bit %#x: %v; want ErrNotImplemented", name, bit, lerr)
			}
		}
	}

	extendedAt := inodeOffset(t, img, "big")
	if binary.LittleEndian.Uint16(img[extendedAt:])&disk.InodeFormatExtended == 0 {
		t.Fatal("the Writer made big a compact inode; the test wants an extended one")
	}

	fsys, err := erofs.Open(bytes.NewReader(withBit(img, compactAt, disk.InodeFormatNlink1)))
	if err != nil {
		t.Fatal(err)
	}

	if _, lerr := fs.Lstat(fsys, "a"); !errors.Is(lerr, erofs.ErrNotImplemented) {
		t.Fatalf("nlink-1 on a compact file: %v; want ErrNotImplemented", lerr)
	}

	fsys, err = erofs.Open(bytes.NewReader(withBit(img, extendedAt, disk.InodeFormatNlink1)))
	if err != nil {
		t.Fatal(err)
	}

	if got, rerr := fs.ReadFile(fsys, "big"); rerr != nil || !bytes.Equal(got, content) {
		t.Fatalf("bit 4 on an extended file: %d bytes, %v; want the content, as the kernel ignores it", len(got), rerr)
	}

	fsys, err = erofs.Open(bytes.NewReader(withBit(img, inodeOffset(t, img, "d"), disk.InodeFormatDotOmitted)))
	if err != nil {
		t.Fatal(err)
	}

	entries, derr := fs.ReadDir(fsys, "d")
	if derr != nil || len(entries) != 1 || entries[0].Name() != "x" {
		t.Fatalf("a dot-omitted directory: %v, %v; want its one entry", entries, derr)
	}

	if got, rerr := fs.ReadFile(fsys, "d/x"); rerr != nil || string(got) != "x" {
		t.Fatalf("a file under a dot-omitted directory: %q, %v", got, rerr)
	}
}

// TestReaderExtendedInodeHighBlockBits: i_nb on an extended inode is the
// start block's high 16 bits, which the kernel always reads; a nonzero
// value is a 48-bit address this reader does not implement, and is refused
// rather than read at the low 32 bits.
func TestReaderExtendedInodeHighBlockBits(t *testing.T) {
	t.Parallel()

	img := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/big", bytes.Repeat([]byte("b"), 3000)) })

	at := inodeOffset(t, img, "big")
	if binary.LittleEndian.Uint16(img[at:])&disk.InodeFormatExtended == 0 {
		t.Fatal("the Writer made big a compact inode; the test wants an extended one")
	}

	img[at+6] = 0x01

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if _, err = fs.ReadFile(fsys, "big"); !errors.Is(err, erofs.ErrNotImplemented) {
		t.Fatalf("i_nb set on an extended inode: %v; want ErrNotImplemented", err)
	}
}

// TestReaderHeaderOnlyXattrBody: an xattr count of one is a body of only
// its header, which the kernel refuses as not supported yet.
func TestReaderHeaderOnlyXattrBody(t *testing.T) {
	t.Parallel()

	img := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/a", []byte("a")) })
	at := inodeOffset(t, img, "a")

	if binary.LittleEndian.Uint16(img[at+2:]) != 0 {
		t.Fatal("the file carries xattrs already")
	}

	binary.LittleEndian.PutUint16(img[at+2:], 1)

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if _, err = fs.Lstat(fsys, "a"); !errors.Is(err, erofs.ErrNotImplemented) {
		t.Fatalf("a header-only xattr body: %v; want ErrNotImplemented", err)
	}
}

// TestReaderStatErrorIsNilInfo: Lstat and Stat return a nil FileInfo
// beside their error, as fs.Lstat and fs.Stat promise, not a typed nil a
// caller's type assertion would then dereference.
func TestReaderStatErrorIsNilInfo(t *testing.T) {
	t.Parallel()

	img := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/a", []byte("a")) })
	at := inodeOffset(t, img, "a")
	binary.LittleEndian.PutUint16(img[at+2:], 1)

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if st, err := fs.Lstat(fsys, "a"); err == nil || st != nil {
		t.Fatalf("Lstat on a refused inode: %v, %v; want a nil FileInfo and the error", st, err)
	}

	if st, err := fs.Stat(fsys, "a"); err == nil || st != nil {
		t.Fatalf("Stat on a refused inode: %v, %v; want a nil FileInfo and the error", st, err)
	}
}

func TestReaderFlatPlainNullAddress(t *testing.T) {
	t.Parallel()

	content := []byte("inline content")
	img := buildImage(t, func(w *erofs.Writer) {
		writeFile(t, w, "/a", content)

		if err := w.Symlink("target", "/l"); err != nil {
			t.Fatal(err)
		}
	})

	// The file is inline; make it a compact flat-plain inode at the null
	// address, the kernel's hole (an extended inode's start block is 48
	// bits wide, and the same word there is an address past the device:
	// TestReaderExtendedNullAddress).
	at := inodeOffset(t, img, "a")
	compactHole(img, at, uint32(len(content)))

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	got, err := fs.ReadFile(fsys, "a")
	if err != nil || !bytes.Equal(got, make([]byte, len(content))) {
		t.Fatalf("a null-address file: %q, %v; want %d zeros", got, err, len(content))
	}

	st, err := fs.Stat(fsys, "a")
	if err != nil {
		t.Fatal(err)
	}

	ranges := st.(interface{ DataRange() []erofs.DataRange }).DataRange()
	if len(ranges) != 1 || ranges[0].Offset != -1 || ranges[0].Size != int64(len(content)) {
		t.Fatalf("a null-address file's ranges: %+v; want one hole of %d", ranges, len(content))
	}

	// A flat-plain symlink at the null address is refused: on this
	// extended inode the address lies past the device, and on a compact
	// one the target would be zeros, the empty string to the kernel.
	at = inodeOffset(t, img, "l")
	format := binary.LittleEndian.Uint16(img[at:])
	binary.LittleEndian.PutUint16(img[at:], format&^disk.InodeFormatLayoutMask|disk.LayoutFlatPlain<<1)
	binary.LittleEndian.PutUint32(img[at+16:], disk.NullAddr)

	fsys, err = erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if target, err := fs.ReadLink(fsys, "l"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("a null-address symlink: %q, %v; want ErrCorrupt", target, err)
	}
}

func TestReaderFIFOHasNoDevice(t *testing.T) {
	t.Parallel()

	img := buildImage(t, func(w *erofs.Writer) {
		if err := w.Mknod("/p", fs.ModeNamedPipe|0o600, 0); err != nil {
			t.Fatal(err)
		}

		if err := w.Mknod("/s", fs.ModeSocket|0o600, 0); err != nil {
			t.Fatal(err)
		}

		if err := w.Mknod("/c", fs.ModeCharDevice|fs.ModeDevice|0o600, 0x0105); err != nil {
			t.Fatal(err)
		}
	})

	for _, name := range []string{"p", "s"} {
		at := inodeOffset(t, img, name)
		binary.LittleEndian.PutUint32(img[at+16:], 0xFFFFFFFF)
	}

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]uint64{"p": 0, "s": 0, "c": 0x0105} {
		st, err := fs.Stat(fsys, name)
		if err != nil {
			t.Fatal(err)
		}

		if rdev := st.(interface{ Rdev() uint64 }).Rdev(); rdev != want {
			t.Fatalf("%s: Rdev %d; want %d", name, rdev, want)
		}
	}
}

// TestReaderBlockMapChunkRanges: a chunk-based file whose map is the
// four-byte block array (what mkfs.erofs writes for --chunksize on the
// primary device) reports ranges that cover its size and point at its
// bytes, as the eight-byte index form does. The image is mkfs.erofs 1.9.4
// over a small tree with -b512 --chunksize=512; the Writer never emits this
// map form.
func TestReaderBlockMapChunkRanges(t *testing.T) {
	t.Parallel()

	img, err := os.ReadFile("internal/corpus/testdata/small-chunk-512.erofs")
	if err != nil {
		t.Fatal(err)
	}

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	files := 0

	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		st, err := fs.Lstat(fsys, p)
		if err != nil {
			return err
		}

		content, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}

		var got []byte

		for _, r := range st.(interface{ DataRange() []erofs.DataRange }).DataRange() {
			if r.Offset < 0 {
				got = append(got, make([]byte, r.Size)...)
				continue
			}

			got = append(got, img[r.Offset:r.Offset+r.Size]...)
		}

		if !bytes.Equal(got, content) {
			t.Errorf(
				"%s: the ranges read %d bytes %q; the file is %d bytes %q",
				p,
				len(got),
				got,
				len(content),
				content,
			)
		}

		files++

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if files < 10 {
		t.Fatalf("%d regular files in the image; the tree has more", files)
	}
}

// rangesRead reads a file's DataRange entries from the image and its extra
// devices, so a range names the bytes it claims to.
func rangesRead(t *testing.T, st fs.FileInfo, devices [][]byte) []byte {
	t.Helper()

	var got []byte

	for _, r := range st.(interface{ DataRange() []erofs.DataRange }).DataRange() {
		if r.Offset < 0 {
			got = append(got, make([]byte, r.Size)...)
			continue
		}

		if int(r.Device) >= len(devices) {
			t.Fatalf("%s: range %+v names a device the image has not", st.Name(), r)
		}

		dev := devices[r.Device]
		if r.Offset+r.Size > int64(len(dev)) {
			t.Fatalf("%s: range %+v past device %d's %d bytes", st.Name(), r, r.Device, len(dev))
		}

		got = append(got, dev[r.Offset:r.Offset+r.Size]...)
	}

	return got
}

// TestReaderBlobDeviceRanges: on an image with a blob device, a chunk
// whose index names the primary device at an address inside the blob's
// mapped range (the slot's mapped_blkaddr and block count) is the blob's, as
// erofs_map_dev places it; DataRange names that device and offset, as the
// readers already did, instead of an offset past the image. The images are
// mkfs.erofs 1.9.4 with -b512 --chunksize=1024 --blobdev over a small tree.
func TestReaderBlobDeviceRanges(t *testing.T) {
	t.Parallel()

	img, err := os.ReadFile("internal/corpus/testdata/small-blobdev-512.erofs")
	if err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile("internal/corpus/testdata/small-blobdev-512.blob")
	if err != nil {
		t.Fatal(err)
	}

	open := func(img []byte) fs.FS {
		fsys, oerr := erofs.Open(bytes.NewReader(img), erofs.WithExtraDevices(bytes.NewReader(blob)))
		if oerr != nil {
			t.Fatal(oerr)
		}

		return fsys
	}

	fsys := open(img)
	files := 0

	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		st, err := fs.Lstat(fsys, p)
		if err != nil {
			return err
		}

		content, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}

		if got := rangesRead(t, st, [][]byte{img, blob}); !bytes.Equal(got, content) {
			t.Errorf("%s: the ranges read %q; the file is %q", p, got, content)
		}

		files++

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if files < 10 {
		t.Fatalf("%d regular files in the image; the tree has more", files)
	}

	// The index of d/sub/e06 (nid 89, at byte 2912) names the blob by id;
	// with the id zeroed its block 13 is the primary device's, and block 13
	// is where the blob's mapped range begins: the kernel reads the blob's
	// first block, and so must the range.
	bad := bytes.Clone(img)
	bad[2914] = 0
	fsys = open(bad)

	st, err := fs.Lstat(fsys, "d/sub/e06")
	if err != nil {
		t.Fatal(err)
	}

	ranges := st.(interface{ DataRange() []erofs.DataRange }).DataRange()
	if len(ranges) != 1 || ranges[0].Device != 1 || ranges[0].Offset != 0 || ranges[0].Size != 1 {
		t.Fatalf("a primary address inside the blob's mapped range: %+v; want {1 0 1}", ranges)
	}

	if got, err := fs.ReadFile(fsys, "d/sub/e06"); err != nil || !bytes.Equal(got, blob[:1]) {
		t.Fatalf("read of that chunk: %q, %v; want the blob's first byte %q", got, err, blob[:1])
	}
}

// TestReaderFlatDataOnMappedDevice: a flat file's blocks are numbered on
// the primary device, and a number inside an extra device's mapped range
// is that device's, as erofs_map_dev places it and mkfs.erofs --blobdev
// writes it; one past every device is corrupt, as is a range that runs off
// the device's end. The image is the blob-device fixture with nid 48 (multi,
// 3000 bytes) turned from chunk-based into a compact flat-plain inode whose
// i_u names the block; the blob maps blocks 13 to 59.
func TestReaderFlatDataOnMappedDevice(t *testing.T) {
	t.Parallel()

	img, err := os.ReadFile("internal/corpus/testdata/small-blobdev-512.erofs")
	if err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile("internal/corpus/testdata/small-blobdev-512.blob")
	if err != nil {
		t.Fatal(err)
	}

	const at = 1536 // nid 48's inode

	flat := func(block uint32) fs.FS {
		bad := bytes.Clone(img)
		bad[at] = 0 // compact, flat-plain
		binary.LittleEndian.PutUint32(bad[at+16:], block)

		fsys, oerr := erofs.Open(bytes.NewReader(bad), erofs.WithExtraDevices(bytes.NewReader(blob)))
		if oerr != nil {
			t.Fatal(oerr)
		}

		return fsys
	}

	// Block 33 is the blob's block 20: byte 10240 of it.
	fsys := flat(33)

	got, err := fs.ReadFile(fsys, "multi")
	if err != nil || !bytes.Equal(got, blob[10240:13240]) {
		t.Fatalf("a flat file on the mapped device: %d bytes, %v; want the blob's bytes 10240 to 13240", len(got), err)
	}

	st, err := fs.Lstat(fsys, "multi")
	if err != nil {
		t.Fatal(err)
	}

	ranges := st.(interface{ DataRange() []erofs.DataRange }).DataRange()
	if len(ranges) != 1 || ranges[0] != (erofs.DataRange{Device: 1, Offset: 10240, Size: 3000}) {
		t.Fatalf("its ranges: %+v; want {1 10240 3000}", ranges)
	}

	for _, block := range []uint32{58, 60, 1000} {
		fsys = flat(block)

		if _, rerr := fs.ReadFile(fsys, "multi"); !errors.Is(rerr, erofs.ErrCorrupt) {
			t.Fatalf("a flat file at block %d: %v; want ErrCorrupt", block, rerr)
		}

		if _, lerr := fs.Lstat(fsys, "multi"); !errors.Is(lerr, erofs.ErrCorrupt) {
			t.Fatalf("Lstat of a flat file at block %d: %v; want ErrCorrupt", block, lerr)
		}
	}
}

// TestReaderUnsupportedDatalayout: an i_format naming a data layout the
// kernel does not define (5 to 7) or one this reader does not implement
// (the compressed ones, 1 and 3) is ErrNotImplemented when the inode is
// read, as the kernel refuses the undefined ones (inode.c, "unsupported
// datalayout") and as the package's doc promises for compressed inodes,
// not a file served until its data is asked for: an empty compressed file
// has no data to ask for.
func TestReaderUnsupportedDatalayout(t *testing.T) {
	t.Parallel()

	img := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/a", []byte("a")) })
	at := inodeOffset(t, img, "a")

	for _, layout := range []uint16{
		disk.LayoutCompressedFull, disk.LayoutCompressedCompact, disk.LayoutMax, disk.LayoutMax + 1, disk.LayoutMax + 2,
	} {
		bad := bytes.Clone(img)
		format := binary.LittleEndian.Uint16(bad[at:])
		binary.LittleEndian.PutUint16(bad[at:], format&^disk.InodeFormatLayoutMask|layout<<1)
		stampChecksum(bad)

		fsys, err := erofs.Open(bytes.NewReader(bad))
		if err != nil {
			t.Fatalf("layout %d: Open: %v", layout, err)
		}

		if _, err := fs.Stat(fsys, "a"); !errors.Is(err, erofs.ErrNotImplemented) {
			t.Fatalf("layout %d: Stat: %v; want ErrNotImplemented", layout, err)
		}

		if _, err := fs.ReadFile(fsys, "a"); !errors.Is(err, erofs.ErrNotImplemented) {
			t.Fatalf("layout %d: ReadFile: %v; want ErrNotImplemented", layout, err)
		}
	}

	// The fuzzer's find: b512 with one inode's i_format set to 0x17, an
	// empty compressed-compact inode, served by Stat before.
	for _, im := range corpus.Committed(t) {
		if im.Name != "b512" {
			continue
		}

		edited := bytes.Clone(im.Img)
		edited[4544] = 0x17

		fsys, err := erofs.Open(bytes.NewReader(edited))
		if err != nil {
			t.Fatal(err)
		}

		refused := 0

		err = fs.WalkDir(fsys, ".", func(name string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			switch _, err := fs.Lstat(fsys, name); {
			case errors.Is(err, erofs.ErrNotImplemented):
				refused++
			case err != nil:
				return err
			}

			return nil
		})
		if err != nil || refused != 1 {
			t.Fatalf(
				"b512 with a compressed inode: walk %v, %d refused; want one ErrNotImplemented and nothing else",
				err,
				refused,
			)
		}
	}
}

// TestReaderExtendedNullAddress: the null address names a hole on a compact
// inode only; on an extended inode the start block is 48 bits wide
// (startblk_lo under i_nb, inode.c's addrmask), so 0xffffffff there is
// block 0xffffffff, past the device, refused at Stat as every address past
// the device is. The first input is the fuzzer's: small-b512's d/sub/e16,
// an all-inline file whose i_u holds the null address, with its layout
// turned to flat-plain.
func TestReaderExtendedNullAddress(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Name != "small-b512" {
			continue
		}

		edited := bytes.Clone(im.Img)
		edited[6720] = 0x01

		fsys, err := erofs.Open(bytes.NewReader(edited))
		if err != nil {
			t.Fatal(err)
		}

		if _, err := fs.Stat(fsys, "d/sub/e16"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("small-b512 d/sub/e16 as flat-plain at the null address: Stat %v; want ErrCorrupt", err)
		}
	}

	img := buildImage(t, func(w *erofs.Writer) { writeFile(t, w, "/a", []byte("a")) })
	at := inodeOffset(t, img, "a")

	edited := bytes.Clone(img)
	format := binary.LittleEndian.Uint16(edited[at:])
	binary.LittleEndian.PutUint16(edited[at:], format&^disk.InodeFormatLayoutMask|disk.LayoutFlatPlain<<1)
	binary.LittleEndian.PutUint32(edited[at+16:], disk.NullAddr)
	stampChecksum(edited)

	fsys, err := erofs.Open(bytes.NewReader(edited))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fs.Stat(fsys, "a"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("an extended flat-plain inode at the null address: Stat %v; want ErrCorrupt", err)
	}

	if _, err := fs.ReadFile(fsys, "a"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("an extended flat-plain inode at the null address: ReadFile %v; want ErrCorrupt", err)
	}
}

// TestReaderChunkPastDevice: a chunk index whose range runs past its
// device's declared blocks is ErrCorrupt at Stat, the counterpart of the
// bound a flat range meets, and a read of the file is ErrCorrupt rather
// than a clean end at offset zero, which a consumer would take for an empty
// file. The input is the fuzzer's: small-blobdev-512 with d/sub/e08's one
// chunk index moved to a block far past the blob's 47.
func TestReaderChunkPastDevice(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Name != "small-blobdev-512" {
			continue
		}

		edited := bytes.Clone(im.Img)
		edited[3110] = 0x5a

		fsys, err := erofs.Open(bytes.NewReader(edited), erofs.WithExtraDevices(bytes.NewReader(im.Blob)))
		if err != nil {
			t.Fatal(err)
		}

		if _, err := fs.Stat(fsys, "d/sub/e08"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("Stat of a chunk past its device: %v; want ErrCorrupt", err)
		}

		link, oerr := fsys.Open("d/sub/e08")
		if oerr != nil {
			t.Fatalf("Open: %v", oerr)
		}

		var buf [1]byte

		if n, err := link.Read(buf[:]); !errors.Is(err, erofs.ErrCorrupt) || n != 0 {
			t.Fatalf("Read of a chunk past its device: %d, %v; want 0 and ErrCorrupt", n, err)
		}

		if _, err := fs.ReadFile(fsys, "d/sub/e08"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("ReadFile of a chunk past its device: %v; want ErrCorrupt", err)
		}
	}
}

// TestReaderFastSymlinkTarget: a fast symlink whose inline target holds a
// NUL, or is empty, is refused with its inode, as the kernel's
// erofs_fill_symlink refuses it ("invalid fast symlink size"), so Lstat is
// ErrCorrupt and a metadata-only copy of the image refuses too, not
// ReadLink alone. The
// first input is the fuzzer's: small-chunk-512 with the one byte of d/s's
// target set to zero.
func TestReaderFastSymlinkTarget(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Name != "small-chunk-512" {
			continue
		}

		edited := bytes.Clone(im.Img)
		edited[1824] = 0x00

		fsys, err := erofs.Open(bytes.NewReader(edited))
		if err != nil {
			t.Fatal(err)
		}

		if _, err := fs.Lstat(fsys, "d/s"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("Lstat of a fast symlink with a NUL in its target: %v; want ErrCorrupt", err)
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(fsys, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("CopyFrom(MetadataOnly) of an image with such a symlink: %v; want ErrCorrupt", err)
		}
	}

	img := buildImage(t, func(w *erofs.Writer) {
		if err := w.Symlink("ab", "/l"); err != nil {
			t.Fatal(err)
		}
	})

	// The Writer packs the target beside its extended inode, which carries
	// no xattrs here.
	at := inodeOffset(t, img, "l") + disk.SizeInodeExtended
	if string(img[at:at+2]) != "ab" {
		t.Fatalf("test premise broken: the target beside the inode reads %q", img[at:at+2])
	}

	img[at+1] = 0

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if _, lerr := fs.Lstat(fsys, "l"); !errors.Is(lerr, erofs.ErrCorrupt) {
		t.Fatalf("Lstat of a Writer symlink given a NUL: %v; want ErrCorrupt", lerr)
	}

	// The other half of the kernel's rule: a fast symlink of size zero.
	// i_size is the eight bytes at offset 8 of the extended inode.
	empty := buildImage(t, func(w *erofs.Writer) {
		if serr := w.Symlink("ab", "/l"); serr != nil {
			t.Fatal(serr)
		}
	})
	at = inodeOffset(t, empty, "l")
	binary.LittleEndian.PutUint64(empty[at+8:], 0)

	fsys, err = erofs.Open(bytes.NewReader(empty))
	if err != nil {
		t.Fatal(err)
	}

	if _, lerr := fs.Lstat(fsys, "l"); !errors.Is(lerr, erofs.ErrCorrupt) {
		t.Fatalf("Lstat of a fast symlink of size zero: %v; want ErrCorrupt", lerr)
	}
}

// TestReaderDirentNameOffset: a directory block whose first name offset is
// not a whole number of dirents is refused by ReadDir as the kernel's readdir
// refuses it ("invalid de[0].nameoff"), before any entry is served; the
// reader took the quotient as the entry count and served a listing cut
// short. The first input is the fuzzer's: b512 with the first name offset
// of a block of "many" set to 123, which read as ten entries of three
// hundred.
func TestReaderDirentNameOffset(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Name != "b512" {
			continue
		}

		edited := bytes.Clone(im.Img)
		edited[27144] = 0x7b

		fsys, err := erofs.Open(bytes.NewReader(edited))
		if err != nil {
			t.Fatal(err)
		}

		ents, err := fs.ReadDir(fsys, "many")
		if !errors.Is(err, erofs.ErrCorrupt) || !strings.Contains(err.Error(), "invalid de[0].nameoff") ||
			len(ents) != 0 {
			t.Fatalf(
				"b512 many with de[0].nameoff 123: %d entries, %v; want none and the kernel's refusal",
				len(ents),
				err,
			)
		}
	}

	img := buildImage(t, func(w *erofs.Writer) {
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		writeFile(t, w, "/d/x", []byte("x"))
		writeFile(t, w, "/d/y", []byte("y"))
	})

	// The Writer packs a small directory's dirents beside its extended
	// inode, which carries no xattrs here: the first dirent's name offset
	// is the first two bytes after its nid, nid and type.
	at := inodeOffset(t, img, "d") + disk.SizeInodeExtended + 8

	nameOff := binary.LittleEndian.Uint16(img[at:])
	if nameOff != 4*disk.SizeDirent {
		t.Fatalf("test premise broken: de[0].nameoff is %d, want %d for four entries", nameOff, 4*disk.SizeDirent)
	}

	// An odd offset is refused as the kernel's rule, not by what the names
	// at the odd offset happen to hold.
	for _, bad := range []uint16{nameOff + 1, nameOff - 5} {
		edited := bytes.Clone(img)
		binary.LittleEndian.PutUint16(edited[at:], bad)
		stampChecksum(edited)

		fsys, err := erofs.Open(bytes.NewReader(edited))
		if err != nil {
			t.Fatalf("nameoff %d: Open: %v", bad, err)
		}

		if _, err := fs.ReadDir(
			fsys,
			"d",
		); !errors.Is(err, erofs.ErrCorrupt) ||
			!strings.Contains(err.Error(), "invalid de[0].nameoff") {
			t.Fatalf("nameoff %d: ReadDir: %v; want the kernel's refusal", bad, err)
		}
	}
}

// TestReaderShortDirectoryBlock: a directory whose size leaves a last block
// shorter than one dirent is refused by ReadDir as ErrCorrupt, as the
// kernel's readdir refuses it ("bogus dirent" or "invalid de[0].nameoff",
// whatever lies past i_size in the block), where the reader served the
// directory without that block's entries. The first input is the fuzzer's:
// small-b512 with the size of inode 151 set to 6.
func TestReaderShortDirectoryBlock(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Name != "small-b512" {
			continue
		}

		edited := bytes.Clone(im.Img)
		edited[4840], edited[4841] = 0x06, 0x00

		fsys, err := erofs.Open(bytes.NewReader(edited))
		if err != nil {
			t.Fatal(err)
		}

		refused := 0

		err = fs.WalkDir(fsys, ".", func(_ string, _ fs.DirEntry, err error) error {
			if errors.Is(err, erofs.ErrCorrupt) {
				refused++

				return fs.SkipDir
			}

			return err
		})
		if err != nil || refused != 1 {
			t.Fatalf(
				"small-b512 with a six-byte directory: walk %v, %d refused; want one ErrCorrupt and nothing else",
				err,
				refused,
			)
		}
	}

	img := buildImage(t, func(w *erofs.Writer) {
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		writeFile(t, w, "/d/x", []byte("x"))
	})

	// i_size is the eight bytes at offset 8 of the extended inode: a
	// directory of six bytes has one block of six.
	at := inodeOffset(t, img, "d")
	binary.LittleEndian.PutUint64(img[at+8:], 6)

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fs.ReadDir(fsys, "d"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("ReadDir of a six-byte directory: %v; want ErrCorrupt", err)
	}
}

// TestReaderSuperblockExtSlots: the kernel reads the superblock from one
// page and refuses extension slots running past it (super.c: "invalid
// sb_extslots"), so Open refuses them as a corrupt superblock. The room is
// the larger of the block and a 4 KiB page, less the superblock offset: 184
// slots fit a 512-byte or 4 KiB block and 185 do not, while a 64 KiB block
// holds every count the byte can name. The fuzzer's input, small-blobdev-512
// with 204 slots, is committed beside the harness.
func TestReaderSuperblockExtSlots(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		blockSize int
		fits      byte
		refused   byte
	}{
		{blockSize: 512, fits: 184, refused: 185},
		{blockSize: 4096, fits: 184, refused: 185},
		{blockSize: 65536, fits: 255},
	} {
		var buf testBuffer

		w := erofs.Create(&buf, erofs.WithBlockSize(tc.blockSize))
		writeFile(t, w, "/a", []byte("a"))

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		img := buf.Bytes()
		img[disk.SuperBlockOffset+13] = tc.fits
		stampChecksum(img)

		if _, err := erofs.Open(bytes.NewReader(img)); err != nil {
			t.Fatalf("block %d with %d extension slots: %v", tc.blockSize, tc.fits, err)
		}

		if tc.refused == 0 {
			continue
		}

		img[disk.SuperBlockOffset+13] = tc.refused
		stampChecksum(img)

		if _, err := erofs.Open(bytes.NewReader(img)); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("block %d with %d extension slots: %v; want ErrCorrupt", tc.blockSize, tc.refused, err)
		}
	}
}
