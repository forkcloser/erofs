package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
)

// flatInlineContent is the byte at each place of a laidOneFlatInline file.
func flatInlineContent(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i>>8 + i>>16)
	}

	return data
}

// laidOneFlatInline is an image of 4 KiB blocks, laid by hand, whose one file
// "f" is flat-inline: blocks whole blocks at its start block, then tail
// bytes beside its inode, at most a block. A tail of a whole block follows
// an inode that ends its block; a shorter one follows the inode where it
// is. One more block follows the file's, of bytes that are not the file's.
func laidOneFlatInline(blocks, tail int) []byte {
	const bs = 4096

	// The root's inode and its one entry, then the file's inode.
	inode, data := bs+64, 2
	if tail == bs {
		inode, data = 2*bs-32, 3
	}

	img := make([]byte, (data+blocks+1)*bs)
	laidSuper(img, 12, uint32(data+blocks+1))

	for i := (data + blocks) * bs; i < len(img); i++ {
		img[i] = 0xEE
	}

	dir := laidDirBlock([]laidDirent{{nid: uint64(inode-bs) / 32, ftype: laidFtReg, name: "f"}})
	laidCompact(img[bs:], laidFlatInline, laidDir, 2, uint32(len(dir)), 0)
	copy(img[bs+32:], dir)

	content := flatInlineContent(blocks*bs + tail)
	laidCompact(img[inode:], laidFlatInline, laidReg, 1, uint32(len(content)), uint32(data))
	copy(img[data*bs:], content[:blocks*bs])
	copy(img[inode+32:], content[blocks*bs:])

	return img
}

// slowSink is a writer that is only a writer: io.Copy brings it what it
// read, in its own buffer's size.
type slowSink struct{ got []byte }

func (s *slowSink) Write(p []byte) (int, error) {
	s.got = append(s.got, p...)

	return len(p), nil
}

// TestContractReaderReadsAFlatInlineFileInRuns: a flat-inline file, the
// layout mkfs.erofs gives a file that does not end on a block, is its
// blocks in one run and its last block beside its inode. Reading it whole
// costs as many reads of the image for three hundred blocks as for three,
// not one a block; copied out through a buffer, a read for each buffer.
func TestContractReaderReadsAFlatInlineFileInRuns(t *testing.T) {
	t.Parallel()

	reads := map[int]int{}

	for _, blocks := range []int{3, 300} {
		src := &countedReads{data: laidOneFlatInline(blocks, 100)}
		want := flatInlineContent(blocks*4096 + 100)

		fsys, err := erofs.Open(src)
		if err != nil {
			t.Fatal(err)
		}

		src.calls = 0

		got, err := fs.ReadFile(fsys, "f")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%d blocks: ReadFile gives %d bytes, %v; want the file's %d", blocks, len(got), err, len(want))
		}

		reads[blocks] = src.calls

		f, err := fsys.Open("f")
		if err != nil {
			t.Fatal(err)
		}

		src.calls = 0

		var out slowSink

		if _, err := io.Copy(&out, f); err != nil || !bytes.Equal(out.got, want) {
			t.Fatalf("%d blocks: io.Copy gives %d bytes, %v; want the file's %d", blocks, len(out.got), err, len(want))
		}

		// io.Copy's buffer is 32 KiB; three reads more for the inode and the two ends.
		if most := len(want)/(32<<10) + 4; src.calls > most {
			t.Errorf(
				"%d blocks: io.Copy read the image %d times; want %d at most, one a buffer",
				blocks,
				src.calls,
				most,
			)
		}
	}

	if reads[300] != reads[3] {
		t.Errorf("ReadFile read the image %d times for 300 blocks and %d for 3; want the same", reads[300], reads[3])
	}
}

// TestContractReaderFlatInlineFileAtEveryStep: a flat-inline file read in
// steps of any size is its bytes, across the place where its blocks end
// and its inline end begins, with an inline end of a whole block too.
func TestContractReaderFlatInlineFileAtEveryStep(t *testing.T) {
	t.Parallel()

	for _, tail := range []int{1, 100, 4000, 4096} {
		for _, step := range []int{1, 7, 1000, 4096, 5000, 1 << 16} {
			t.Run(fmt.Sprintf("tail %d, step %d", tail, step), func(t *testing.T) {
				t.Parallel()

				want := flatInlineContent(3*4096 + tail)

				f, err := openWithDevices(t, laidOneFlatInline(3, tail)).Open("f")
				if err != nil {
					t.Fatal(err)
				}

				var got []byte

				buf := make([]byte, step)

				for {
					n, err := f.Read(buf)
					got = append(got, buf[:n]...)

					if errors.Is(err, io.EOF) {
						break
					}

					if err != nil {
						t.Fatal(err)
					}
				}

				if !bytes.Equal(got, want) {
					t.Errorf("read %d bytes that are not the file's %d", len(got), len(want))
				}
			})
		}
	}
}

// endingAt is an image whose reader ends before the image does, and does
// not say how long it is.
type endingAt struct {
	data []byte
}

func (e endingAt) ReadAt(p []byte, off int64) (int, error) {
	return bytes.NewReader(e.data).ReadAt(p, off)
}

// TestContractReaderFlatInlineFileOnAShortDevice: a device that ends
// inside a flat-inline file's blocks is the image's fault, ErrCorrupt, and
// never the file's end: by ReadFile, and by Read whatever it had read.
func TestContractReaderFlatInlineFileOnAShortDevice(t *testing.T) {
	t.Parallel()

	img := laidOneFlatInline(8, 100)

	// The image ends in the file's fifth block.
	fsys, err := erofs.Open(endingAt{img[:(2+4)*4096+10]})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = fs.ReadFile(fsys, "f"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("ReadFile: %v; want ErrCorrupt", err)
	}

	f, err := fsys.Open("f")
	if err != nil {
		t.Fatal(err)
	}

	total, buf := 0, make([]byte, 3000)

	for {
		n, err := f.Read(buf)
		total += n

		if err != nil {
			if !errors.Is(err, erofs.ErrCorrupt) || errors.Is(err, io.EOF) {
				t.Errorf("Read after %d bytes: %v; want ErrCorrupt", total, err)
			}

			break
		}
	}

	if total >= 8*4096+100 {
		t.Errorf("Read gave %d bytes of a file whose device ends inside it", total)
	}
}

// askedPastEnd is a source that counts the reads that begin at its end or
// after it.
type askedPastEnd struct {
	data  []byte
	asked int
}

func (s *askedPastEnd) Size() int64 { return int64(len(s.data)) }

func (s *askedPastEnd) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(s.data)) {
		s.asked++

		return 0, io.EOF
	}

	n := copy(p, s.data[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// TestContractReaderHoldsAnInlineEndToTheImage: an inode in the image's
// last 32 bytes has its inline data at the head of the block after it, a
// block the image has not. The file is ErrCorrupt by every way of reading
// it, and the source is asked for nothing at or past its end: a reader
// that indexes its bytes by the offset it is given would be handed one
// outside them.
func TestContractReaderHoldsAnInlineEndToTheImage(t *testing.T) {
	t.Parallel()

	const bs = 4096

	img := make([]byte, 2*bs)
	laidSuper(img, 12, 2)

	// The root's inode and its one entry, then the file's inode, the last
	// 32 bytes of the image.
	inode := 2*bs - 32
	dir := laidDirBlock([]laidDirent{{nid: uint64(inode-bs) / 32, ftype: laidFtReg, name: "f"}})
	laidCompact(img[bs:], laidFlatInline, laidDir, 2, uint32(len(dir)), 0)
	copy(img[bs+32:], dir)
	laidCompact(img[inode:], laidFlatInline, laidReg, 1, 100, 0)

	src := &askedPastEnd{data: img}

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := fs.ReadFile(fsys, "f"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("ReadFile: %d bytes, %v; want ErrCorrupt", len(got), err)
	}

	for _, way := range []string{"Read", "io.Copy"} {
		f, err := fsys.Open("f")
		if err != nil {
			t.Fatal(err)
		}

		var n int64

		if way == "Read" {
			var read int

			read, err = f.Read(make([]byte, 200))
			n = int64(read)
		} else {
			var out bytes.Buffer

			n, err = io.Copy(&out, f)
		}

		if !errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("%s: %d bytes, %v; want ErrCorrupt", way, n, err)
		}
	}

	if src.asked != 0 {
		t.Errorf("the source was asked %d times for bytes at or past its end; want never", src.asked)
	}
}
