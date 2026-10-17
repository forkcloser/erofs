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
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/disk"
)

// The reader follows the kernel's rules where an image departs from what
// mkfs.erofs writes: the superblock checksum is verified before the feature
// bits are read, an i_format bit the kernel does not define is a format this
// reader lacks, a flat-plain file at the null address reads as zeros, and a
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

	// The file is inline; make it flat-plain at the null address, the
	// shape one bit away from what mkfs.erofs writes for every such file.
	at := inodeOffset(t, img, "a")
	format := binary.LittleEndian.Uint16(img[at:])
	binary.LittleEndian.PutUint16(img[at:], format&^disk.InodeFormatLayoutMask|disk.LayoutFlatPlain<<1)
	binary.LittleEndian.PutUint32(img[at+16:], disk.NullAddr)

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

	// The same shape on a symlink is a target of zeros, which the kernel
	// reads as the empty string: refused, as an empty target is.
	at = inodeOffset(t, img, "l")
	format = binary.LittleEndian.Uint16(img[at:])
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
