package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/forkcloser/erofs"
)

const (
	flatInode   = 1536 // nid 48's inode in the blob-device fixture
	flatPlain   = 0
	flatInline  = 2
	modeRegular = 0o100644
	modeSymlink = 0o120777
	slotBlocks  = 47 // the blocks the fixture's device slot declares
)

// flatImage is the blob-device fixture (512-byte blocks, the blob mapped at
// blocks 13 to 59) with nid 48 ("multi", six blocks) turned into a compact
// inode of the given layout, type and size starting at the given block, the
// image padded with 'P' so that it holds blocks up to 70 itself.
func flatImage(t *testing.T, layout uint8, mode uint16, block, size uint32) []byte {
	t.Helper()

	img, err := os.ReadFile("internal/corpus/testdata/small-blobdev-512.erofs")
	if err != nil {
		t.Fatal(err)
	}

	const entryType = 1358 // the type nid 48's directory entry gives it

	img = append(img, bytes.Repeat([]byte("P"), 70*512-len(img))...)
	img[flatInode] = layout << 1
	binary.LittleEndian.PutUint16(img[flatInode+4:], mode)
	binary.LittleEndian.PutUint32(img[flatInode+8:], size)
	binary.LittleEndian.PutUint32(img[flatInode+16:], block)

	if mode == modeSymlink {
		img[entryType] = 7

		stampChecksum(img)
	}

	return img
}

// overBlob opens img over a device of the given blocks, all 'B'.
func overBlob(t *testing.T, img []byte, blocks int) fs.FS {
	t.Helper()

	blob := bytes.Repeat([]byte("B"), blocks*512)

	fsys, err := erofs.Open(bytes.NewReader(img), erofs.WithExtraDevices(bytes.NewReader(blob)))
	if err != nil {
		t.Fatal(err)
	}

	return fsys
}

// TestContractAFlatFileOverTheEdgeOfAMappedRangeIsRefused: a flat file's
// blocks are one run from its start block, and a run that begins inside an
// extra device's mapped range and ends past it, or begins before the range
// and ends inside it, lies on no one device. The kernel translates the
// address a read begins at and reads on from there, so the bytes it serves
// for the far side depend on where its reads happen to begin; this reader
// refuses the file, by every path, and no path serves bytes of two devices.
//
// Each run is read over a device of its slot's 47 blocks and over a longer
// one, which holds bytes where a read carried on past the slot would land.
func TestContractAFlatFileOverTheEdgeOfAMappedRangeIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		layout uint8
		block  uint32
		size   uint32
	}{
		{"out of the range's end", flatPlain, 58, 3000},                          // blocks 58 to 63
		{"out of the range's end by its last byte", flatPlain, 55, 5*512 + 1},    // 55 to 59 and a byte
		{"into the range's start", flatPlain, 10, 3000},                          // blocks 10 to 15
		{"into the range's start by its last byte", flatPlain, 8, 5*512 + 1},     // 8 to 12 and a byte
		{"an inline file's last whole block past the end", flatInline, 56, 3000}, // blocks 56 to 60
	} {
		for _, blocks := range []int{slotBlocks, slotBlocks + 8} {
			t.Run(fmt.Sprintf("%s, a device of %d blocks", tc.name, blocks), func(t *testing.T) {
				t.Parallel()

				fsys := overBlob(t, flatImage(t, tc.layout, modeRegular, tc.block, tc.size), blocks)

				if _, err := fs.Lstat(fsys, "multi"); !errors.Is(err, erofs.ErrCorrupt) {
					t.Errorf("Lstat: %v; want ErrCorrupt", err)
				}

				if got, err := fs.ReadFile(fsys, "multi"); !errors.Is(err, erofs.ErrCorrupt) {
					t.Errorf("ReadFile: %d bytes, %v; want ErrCorrupt", len(got), err)
				}

				f, err := fsys.Open("multi")
				if err == nil {
					_, err = io.ReadAll(f)

					if cerr := f.Close(); cerr != nil {
						t.Errorf("Close: %v", cerr)
					}
				}

				if !errors.Is(err, erofs.ErrCorrupt) {
					t.Errorf("Open and read to the end: %v; want ErrCorrupt", err)
				}
			})
		}
	}
}

// TestContractASymlinkOverTheEdgeOfAMappedRangeIsRefused: the same run as a
// symbolic link's target, which no Stat judges: 3000 bytes none of which is
// NUL, a target the reader would otherwise hand back.
func TestContractASymlinkOverTheEdgeOfAMappedRangeIsRefused(t *testing.T) {
	t.Parallel()

	for _, block := range []uint32{58, 10} {
		for _, blocks := range []int{slotBlocks, slotBlocks + 8} {
			fsys := overBlob(t, flatImage(t, flatPlain, modeSymlink, block, 3000), blocks)

			target, err := fsys.(fs.ReadLinkFS).ReadLink("multi")
			if !errors.Is(err, erofs.ErrCorrupt) {
				t.Errorf(
					"ReadLink of a target at block %d, a device of %d blocks: %d bytes, %v; want ErrCorrupt",
					block, blocks, len(target), err,
				)
			}
		}
	}
}

// TestContractAFlatFileWhollyOnOneDeviceIsRead: the runs beside those the
// tests above refuse, each whole on its device: the range's last six blocks
// are the device's, the six after the range the image's own, and a run that
// ends with the last byte before the range touches nothing of it.
func TestContractAFlatFileWhollyOnOneDeviceIsRead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		block  uint32
		device uint16
		offset int64
	}{
		{"the range's last blocks", 54, 1, 41 * 512},
		{"the blocks after the range", 60, 0, 60 * 512},
		{"the blocks before the range", 7, 0, 7 * 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const size = 6 * 512

			img := flatImage(t, flatPlain, modeRegular, tc.block, size)
			fsys := overBlob(t, img, slotBlocks)

			want := bytes.Repeat([]byte("B"), size)
			if tc.device == 0 {
				want = img[tc.offset : tc.offset+size]
			}

			got, err := fs.ReadFile(fsys, "multi")
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf(
					"ReadFile: %d bytes, %v; want the %d bytes at %d of device %d",
					len(got),
					err,
					size,
					tc.offset,
					tc.device,
				)
			}

			st, err := fs.Lstat(fsys, "multi")
			if err != nil {
				t.Fatal(err)
			}

			ranges := st.(interface{ DataRange() []erofs.DataRange }).DataRange()
			if len(ranges) != 1 || ranges[0] != (erofs.DataRange{Device: tc.device, Offset: tc.offset, Size: size}) {
				t.Fatalf("its ranges: %+v; want one of %d bytes at %d of device %d", ranges, size, tc.offset, tc.device)
			}
		})
	}
}

// TestContractAnInlineFilesBlocksEndingWithAMappedRangeAreRead: a file whose
// last bytes follow its inode has one run of whole blocks, the tail not
// among them: five blocks that are the range's last five lie whole on the
// device, though the file's 3000 bytes from there would not.
func TestContractAnInlineFilesBlocksEndingWithAMappedRangeAreRead(t *testing.T) {
	t.Parallel()

	const head = 5 * 512

	img := flatImage(t, flatInline, modeRegular, 55, 3000)
	fsys := overBlob(t, img, slotBlocks)

	want := append(bytes.Repeat([]byte("B"), head), img[flatInode+32:flatInode+32+3000-head]...)

	got, err := fs.ReadFile(fsys, "multi")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadFile: %d bytes, %v; want five blocks of the device and the bytes after the inode", len(got), err)
	}

	st, err := fs.Lstat(fsys, "multi")
	if err != nil {
		t.Fatal(err)
	}

	ranges := st.(interface{ DataRange() []erofs.DataRange }).DataRange()
	if len(ranges) != 2 || ranges[0] != (erofs.DataRange{Device: 1, Offset: 42 * 512, Size: head}) {
		t.Fatalf("its ranges: %+v; want the device's last five blocks first", ranges)
	}
}

// TestContractAFlatFileThroughASlotsRangeIsReadWithoutItsDevices: opened with
// none of its extra devices, an image is one device, and the kernel
// translates no address of it (erofs_map_dev: extra_devices && !flatdev): a
// flat file's run through a slot's range, over either edge of it or inside
// it, is the image's bytes, read straight through, and its range the
// image's. Each block of the image past the fixture's own holds its number.
func TestContractAFlatFileThroughASlotsRangeIsReadWithoutItsDevices(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		block uint32
	}{
		{"into the range's start", 10},
		{"inside the range", 33},
		{"out of the range's end", 58},
		{"past the range", 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			img := flatImage(t, flatPlain, modeRegular, tc.block, 3000)
			for at := 13 * 512; at < len(img); at++ {
				img[at] = byte(at / 512)
			}

			fsys, err := erofs.Open(bytes.NewReader(img))
			if err != nil {
				t.Fatal(err)
			}

			from := int64(tc.block) * 512

			got, err := fs.ReadFile(fsys, "multi")
			if err != nil || !bytes.Equal(got, img[from:from+3000]) {
				t.Fatalf("ReadFile: %d bytes, %v; want the image's 3000 bytes from block %d", len(got), err, tc.block)
			}

			st, err := fs.Lstat(fsys, "multi")
			if err != nil {
				t.Fatal(err)
			}

			ranges := st.(interface{ DataRange() []erofs.DataRange }).DataRange()
			if len(ranges) != 1 || ranges[0] != (erofs.DataRange{Device: 0, Offset: from, Size: 3000}) {
				t.Fatalf("its ranges: %+v; want one of 3000 bytes at %d of the image", ranges, from)
			}
		})
	}
}
