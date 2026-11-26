package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// laidOverMappedRange is an image of eight 4 KiB blocks and its one extra
// device, of the given blocks. The device's slot declares four, mapped at
// the image's block 3: the primary device's blocks 3 to 6 are the device's
// 0 to 3. The one file "f" is chunk-based, format its chunk format, with a
// chunk at each of the starts, all addresses of the primary device. Every
// byte of the image's block b is 0x10+b, and of the device's, 0xD0+b.
func laidOverMappedRange(blocks int, format uint32, starts ...uint32) (img, device []byte) {
	const bs = 4096

	img = make([]byte, 8*bs)
	for b := 2; b < 8; b++ {
		for i := b * bs; i < (b+1)*bs; i++ {
			img[i] = byte(0x10 + b)
		}
	}

	laidSuper(img, 12, 8)

	sb := img[1024:]
	binary.LittleEndian.PutUint32(sb[80:], 0x4|0x8) // INCOMPAT_CHUNKED_FILE, INCOMPAT_DEVICE_TABLE
	binary.LittleEndian.PutUint16(sb[86:], 1)       // extra_devices
	binary.LittleEndian.PutUint16(sb[88:], 16)      // devt_slotoff, in slots of 128 bytes

	binary.LittleEndian.PutUint32(img[16*128+64:], 4) // the slot's block count
	binary.LittleEndian.PutUint32(img[16*128+68:], 3) // and where it is mapped

	root := laidDirBlock([]laidDirent{{0, laidFtDir, "."}, {0, laidFtDir, ".."}, {3, laidFtReg, "f"}})
	laidCompact(img[bs:], laidFlatInline, laidDir, 2, uint32(len(root)), 0)
	copy(img[bs+32:], root)

	chunk := uint64(bs) << (format & 0x1F)
	laidExtended(img[bs+96:], laidChunked, laidReg, uint64(len(starts))*chunk, format, 1)

	for i, start := range starts {
		if format&laidIndexes != 0 {
			binary.LittleEndian.PutUint32(img[laidMapOff+i*8+4:], start)
		} else {
			binary.LittleEndian.PutUint32(img[laidMapOff+i*4:], start)
		}
	}

	device = make([]byte, blocks*bs)
	for i := range device {
		device[i] = byte(0xD0 + i/bs)
	}

	return img, device
}

// blocksOf names the block each 4 KiB of data is: its first byte, which
// must be its every byte.
func blocksOf(data []byte) string {
	var out []byte

	for at := 0; at < len(data); at += 4096 {
		end := min(at+4096, len(data))
		if bytes.Count(data[at:end], data[at:at+1]) != end-at {
			out = append(out, "mixed "...)

			continue
		}

		out = fmt.Appendf(out, "%02X ", data[at])
	}

	return string(bytes.TrimSpace(out))
}

// readWay is what one way of reading a file gave: its bytes, and the error
// it ended on.
type readWay struct {
	way string
	got []byte
	err error
}

// readEveryWay is "f" by each way a file is read: whole, copied out, and in
// steps of a hundred bytes; io.EOF is not an error of the last.
func readEveryWay(t *testing.T, fsys fs.FS) []readWay {
	t.Helper()

	whole, wholeErr := fs.ReadFile(fsys, "f")

	f, err := fsys.Open("f")
	if err != nil {
		t.Fatal(err)
	}

	var sink bytes.Buffer

	_, copyErr := io.Copy(&sink, f)

	if f, err = fsys.Open("f"); err != nil {
		t.Fatal(err)
	}

	stepped, buf := []byte{}, make([]byte, 100)

	for err == nil {
		var n int

		n, err = f.Read(buf)
		stepped = append(stepped, buf[:n]...)
	}

	if errors.Is(err, io.EOF) {
		err = nil
	}

	return []readWay{
		{"ReadFile", whole, wholeErr},
		{"io.Copy", sink.Bytes(), copyErr},
		{"Read by a hundred bytes", stepped, err},
	}
}

// TestContractAChunkIsReadWhereItsFirstAddressFalls: a chunk lies on the
// device its first address falls on, whole: the kernel maps a chunk, then
// translates the one address it begins at (erofs_map_chunks, then
// erofs_map_dev). So a chunk that begins on the last block of an extra
// device's mapped range is that device's past the range, a chunk that
// begins a block before the range is the image's into it, and of chunks
// that follow each other across the range's end each is where its own
// first address falls. Every way of reading the file gives those bytes,
// the ones Stat's ranges name, and a copy carries them.
//
// Reads placed each block by the block's own address, or a run of chunks
// by its first one: the bytes read were not the ones the ranges name.
func TestContractAChunkIsReadWhereItsFirstAddressFalls(t *testing.T) {
	t.Parallel()

	const twoBlocks = 1 // the chunk format's bits: a chunk of two blocks

	for _, tc := range []struct {
		name   string
		format uint32
		starts []uint32
		want   string
		ranges []erofs.DataRange
	}{
		{
			"a block map, the second chunk out of the range by its second block",
			twoBlocks,
			[]uint32{3, 6},
			"D0 D1 D3 D4",
			[]erofs.DataRange{{Device: 1, Offset: 0, Size: 8192}, {Device: 1, Offset: 3 * 4096, Size: 8192}},
		},
		{
			"indexes, the second chunk out of the range by its second block",
			twoBlocks | laidIndexes,
			[]uint32{3, 6},
			"D0 D1 D3 D4",
			[]erofs.DataRange{{Device: 1, Offset: 0, Size: 8192}, {Device: 1, Offset: 3 * 4096, Size: 8192}},
		},
		{
			"indexes, one chunk out of the range by its second block",
			twoBlocks | laidIndexes,
			[]uint32{6},
			"D3 D4",
			[]erofs.DataRange{{Device: 1, Offset: 3 * 4096, Size: 8192}},
		},
		{
			"indexes, three chunks in a row, the third past the range",
			laidIndexes,
			[]uint32{5, 6, 7},
			"D2 D3 17",
			[]erofs.DataRange{{Device: 1, Offset: 2 * 4096, Size: 8192}, {Device: 0, Offset: 7 * 4096, Size: 4096}},
		},
		{
			"a block map, three chunks in a row, the third past the range",
			0,
			[]uint32{5, 6, 7},
			"D2 D3 17",
			[]erofs.DataRange{{Device: 1, Offset: 2 * 4096, Size: 8192}, {Device: 0, Offset: 7 * 4096, Size: 4096}},
		},
		{
			"a block map, a chunk that begins a block before the range",
			twoBlocks,
			[]uint32{2},
			"12 13",
			[]erofs.DataRange{{Device: 0, Offset: 2 * 4096, Size: 8192}},
		},
		{
			"indexes, a chunk that begins a block before the range",
			twoBlocks | laidIndexes,
			[]uint32{2},
			"12 13",
			[]erofs.DataRange{{Device: 0, Offset: 2 * 4096, Size: 8192}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			img, device := laidOverMappedRange(6, tc.format, tc.starts...)
			fsys := openWithDevices(t, img, device)

			info, err := fs.Stat(fsys, "f")
			if err != nil {
				t.Fatal(err)
			}

			ranges := info.(interface{ DataRange() []erofs.DataRange }).DataRange()
			if fmt.Sprint(ranges) != fmt.Sprint(tc.ranges) {
				t.Errorf("DataRange: %+v; want %+v", ranges, tc.ranges)
			}

			// What the ranges name, read by hand.
			var named []byte

			for _, r := range ranges {
				from := img
				if r.Device == 1 {
					from = device
				}

				named = append(named, from[r.Offset:r.Offset+r.Size]...)
			}

			if blocksOf(named) != tc.want {
				t.Errorf("the ranges name the blocks %s; want %s", blocksOf(named), tc.want)
			}

			for _, r := range readEveryWay(t, fsys) {
				if r.err != nil || blocksOf(r.got) != tc.want {
					t.Errorf("%s: the blocks %s, %v; want %s", r.way, blocksOf(r.got), r.err, tc.want)
				}
			}

			// A copy, the data with it.
			out := &testBuffer{}
			w := erofs.Create(out)

			if err = errors.Join(w.CopyFrom(fsys), w.Close()); err != nil {
				t.Fatal(err)
			}

			copied, err := fs.ReadFile(openWithDevices(t, out.Bytes()), "f")
			if err != nil || blocksOf(copied) != tc.want {
				t.Errorf("a copy: the blocks %s, %v; want %s", blocksOf(copied), err, tc.want)
			}
		})
	}
}

// TestContractAChunkPastItsDeviceIsNotReadFromAnother: the chunk above that
// begins on the range's last block, over a device of the four blocks its
// slot declares. Its second block is past the device's end: Stat refuses
// the file, and every way of reading it ends in ErrCorrupt after the three
// blocks the device holds. The block was read from the image, at the
// address after the range, and the file served whole.
func TestContractAChunkPastItsDeviceIsNotReadFromAnother(t *testing.T) {
	t.Parallel()

	for _, format := range []uint32{1, 1 | laidIndexes} {
		img, device := laidOverMappedRange(4, format, 3, 6)
		fsys := openWithDevices(t, img, device)

		if _, err := fs.Stat(fsys, "f"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("format %#x: Stat: %v; want ErrCorrupt", format, err)
		}

		for _, r := range readEveryWay(t, fsys) {
			if !errors.Is(r.err, erofs.ErrCorrupt) {
				t.Errorf("format %#x: %s: the blocks %s, %v; want ErrCorrupt", format, r.way, blocksOf(r.got), r.err)
			}

			// ReadFile gives nothing of a file it does not give whole.
			if want := "D0 D1 D3"; r.way != "ReadFile" && blocksOf(r.got) != want {
				t.Errorf(
					"format %#x: %s: the blocks %s before the error; want %s",
					format,
					r.way,
					blocksOf(r.got),
					want,
				)
			}
		}
	}
}

// TestContractA48BitChunkMapIsNotImplemented: a chunk format with the
// 48-bit flag is one the kernel defines and this package does not read:
// Stat and every way of reading the file say ErrNotImplemented, for a block
// map and for indexes. Read as the 32 bits it reads otherwise, the map here
// is a file of block 0.
func TestContractA48BitChunkMapIsNotImplemented(t *testing.T) {
	t.Parallel()

	const flag48 = 0x40

	for _, format := range []uint32{flag48, flag48 | laidIndexes} {
		fsys, err := erofs.Open(laidChunkedFile(4, format))
		if err != nil {
			t.Fatal(err)
		}

		if _, err := fs.Stat(fsys, "f"); !errors.Is(err, erofs.ErrNotImplemented) {
			t.Errorf("format %#x: Stat: %v; want ErrNotImplemented", format, err)
		}

		for _, r := range readEveryWay(t, fsys) {
			if !errors.Is(r.err, erofs.ErrNotImplemented) || len(r.got) != 0 {
				t.Errorf(
					"format %#x: %s: %d bytes, %v; want none and ErrNotImplemented",
					format,
					r.way,
					len(r.got),
					r.err,
				)
			}
		}
	}
}
