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

// inlineFiles is an image of files "f00000".. of size bytes each, small
// enough that the image stores each beside its inode, and each of its own
// bytes.
func inlineFiles(t *testing.T, files, size int) []byte {
	t.Helper()

	out := &testBuffer{}
	w := erofs.Create(out)

	for i := range files {
		writeFile(t, w, fmt.Sprintf("/f%05d", i), inlineContent(i, size))
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// In blocks of their own, the files alone would be 4096 bytes each,
	// after the image's first two blocks.
	if len(out.Bytes()) > 8192+files*2048 {
		t.Fatalf("the image is %d bytes for %d files of %d: the test wants them beside their inodes",
			len(out.Bytes()), files, size)
	}

	return out.Bytes()
}

// inlineContent is file i's size bytes, found nowhere else in its image.
func inlineContent(i, size int) []byte {
	return bytes.Repeat(fmt.Appendf(nil, "<file %05d>", i), size/12+1)[:size]
}

// TestContractMetadataOnlyCopyHoldsNoFileData: a metadata-only copy leaves
// a file's blocks in its image, and copies the end of a file that the image
// stores beside the inode, shorter than a block. It holds none of those
// bytes until Close reads them: four thousand files of 500 bytes cost the
// copy under 64 bytes a file more than four thousand of 5.
//
//nolint:paralleltest // measures the process's live heap; a parallel test's would count in it
func TestContractMetadataOnlyCopyHoldsNoFileData(t *testing.T) {
	const files = 4000

	short := heldByCopy(t, inlineFiles(t, files, 5), erofs.MetadataOnly())
	long := heldByCopy(t, inlineFiles(t, files, 500), erofs.MetadataOnly())

	if extra := long - short; extra > files*64 {
		t.Errorf("files of 500 bytes cost the copy %d bytes more than files of 5, %d a file; want under 64 a file",
			extra, extra/files)
	}
}

// errSourceNoMore is what a laterSource answers once it is gone.
var errSourceNoMore = errors.New("laterSource: the source is gone")

// laterSource is an image a test changes after a copy has read it: gone,
// its reads fail; ended, they find nothing.
type laterSource struct {
	data        []byte
	gone, ended bool
}

func (s *laterSource) ReadAt(p []byte, off int64) (int, error) {
	switch {
	case s.gone:
		return 0, errSourceNoMore
	case s.ended:
		return 0, io.EOF
	}

	return bytes.NewReader(s.data).ReadAt(p, off)
}

// TestContractMetadataOnlyCopyReadsAnInlineEndAtClose: the end of a file
// that a metadata-only copy takes from beside the inode is the file's data,
// and the source's until Close, as the data of any copied file is: Close
// reads it then. A source as it was gives the file its bytes; one changed
// since gives what it holds at Close; one that fails or has ended by then
// fails Close, with its error or as a file cut short.
func TestContractMetadataOnlyCopyReadsAnInlineEndAtClose(t *testing.T) {
	t.Parallel()

	const (
		files = 3
		size  = 500
	)

	img := inlineFiles(t, files, size)

	copied := func(t *testing.T, change func(*laterSource)) ([]byte, []byte, error) {
		t.Helper()

		src := &laterSource{data: bytes.Clone(img)}

		from, err := erofs.Open(src)
		if err != nil {
			t.Fatal(err)
		}

		out := &testBuffer{}
		w := erofs.Create(out)

		if err = w.CopyFrom(from, erofs.MetadataOnly()); err != nil {
			t.Fatal(err)
		}

		change(src)

		err = w.Close()

		return out.Bytes(), src.data, err
	}

	t.Run("the source as it was", func(t *testing.T) {
		t.Parallel()

		out, src, err := copied(t, func(*laterSource) {})
		if err != nil {
			t.Fatal(err)
		}

		fsys := openWithDevices(t, out, src)

		for i := range files {
			if got, err := fs.ReadFile(
				fsys,
				fmt.Sprintf("f%05d", i),
			); err != nil ||
				!bytes.Equal(got, inlineContent(i, size)) {
				t.Errorf("f%05d reads %d bytes, %v; want its own %d", i, len(got), err, size)
			}
		}
	})

	t.Run("the source changed since the copy", func(t *testing.T) {
		t.Parallel()

		out, src, err := copied(t, func(s *laterSource) {
			at := bytes.Index(s.data, inlineContent(1, size))
			if at < 0 {
				t.Fatal("the second file's bytes are not in the image")
			}

			copy(s.data[at:], "changed")
		})
		if err != nil {
			t.Fatal(err)
		}

		want := inlineContent(1, size)
		copy(want, "changed")

		if got, err := fs.ReadFile(openWithDevices(t, out, src), "f00001"); err != nil || !bytes.Equal(got, want) {
			t.Errorf("the file reads %.20q.., %v; want what the source held at Close, %.20q..", got, err, want)
		}
	})

	t.Run("the source gone", func(t *testing.T) {
		t.Parallel()

		if _, _, err := copied(t, func(s *laterSource) { s.gone = true }); !errors.Is(err, errSourceNoMore) {
			t.Errorf("Close: %v; want the source's error", err)
		}
	})

	t.Run("the source ended", func(t *testing.T) {
		t.Parallel()

		if _, _, err := copied(t, func(s *laterSource) { s.ended = true }); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("Close: %v; want ErrUnexpectedEOF", err)
		}
	})
}

// TestContractCloseReadsInlineEndsAWindowAtATime: the inline ends of a
// directory's files lie one after another in their image, and Close reads
// them as they lie, a window of the image at a time: four thousand files
// of 500 bytes, four megabytes of image, in under eighty reads, of little
// more than the image once.
func TestContractCloseReadsInlineEndsAWindowAtATime(t *testing.T) {
	t.Parallel()

	const files = 4000

	src := &countedReads{data: inlineFiles(t, files, 500)}

	from, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	out := &testBuffer{}
	w := erofs.Create(out)

	if err := w.CopyFrom(from, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	src.calls, src.bytes = 0, 0

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if src.calls > files/50 || src.bytes > len(src.data)*11/10 {
		t.Errorf(
			"Close read the image %d times, %d bytes of its %d; want under %d reads, of little more than the image",
			src.calls,
			src.bytes,
			len(src.data),
			files/50,
		)
	}

	fsys := openWithDevices(t, out.Bytes(), src.data)

	for _, i := range []int{0, 1, files / 2, files - 1} {
		if got, err := fs.ReadFile(
			fsys,
			fmt.Sprintf("f%05d", i),
		); err != nil ||
			!bytes.Equal(got, inlineContent(i, 500)) {
			t.Errorf("f%05d reads %d bytes, %v; want its own 500", i, len(got), err)
		}
	}
}

// TestContractInlineEndsOfTwoImagesAreEachTheirOwn: two images laid out
// alike, copied into one result, have their files' inline ends at the same
// offsets: each file gets its own image's bytes.
func TestContractInlineEndsOfTwoImagesAreEachTheirOwn(t *testing.T) {
	t.Parallel()

	layer := func(name, content string) []byte {
		made := &testBuffer{}
		w := erofs.Create(made)
		writeFile(t, w, "/"+name, []byte(content))

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		return made.Bytes()
	}

	first := layer("one", "the first image's bytes")
	second := layer("two", "the second one's, as long")

	out := &testBuffer{}
	w := erofs.Create(out)

	if err := errors.Join(
		w.CopyFrom(openWithDevices(t, first), erofs.MetadataOnly()),
		w.CopyFrom(openWithDevices(t, second), erofs.MetadataOnly()),
		w.Close(),
	); err != nil {
		t.Fatal(err)
	}

	fsys := openWithDevices(t, out.Bytes(), first, second)

	for name, want := range map[string]string{"one": "the first image's bytes", "two": "the second one's, as long"} {
		if got, err := fs.ReadFile(fsys, name); err != nil || string(got) != want {
			t.Errorf("%s reads %q, %v; want %q", name, got, err, want)
		}
	}
}

// laidInlineFiles is an image of 64 KiB blocks, laid by hand: a root that
// lists n files "f000".., each a compact inode with size bytes of its own
// beside it, in the image's second and last block. The files' inodes are
// in the order of their names, or in the reverse of it.
func laidInlineFiles(n, size int, reversed bool) []byte {
	const (
		bs   = 1 << 16
		slot = 32
	)

	// An inode and its bytes, in slots of 32.
	each := (32 + size + slot - 1) / slot * slot

	ents := make([]laidDirent, n)
	for i := range ents {
		ents[i] = laidDirent{ftype: laidFtReg, name: fmt.Sprintf("f%03d", i)}
	}

	dir := len(laidDirBlock(ents))
	first := (32 + dir + slot - 1) / slot * slot

	head := make([]byte, 2*bs)
	laidSuper(head, 16, 2)

	for i := range ents {
		at := i
		if reversed {
			at = n - 1 - i
		}

		off := first + at*each
		ents[i].nid = uint64(off / slot)
		laidCompact(head[bs+off:], laidFlatInline, laidReg, 1, uint32(size), 0)
		copy(head[bs+off+32:], inlineContent(i, size))
	}

	laidCompact(head[bs:], laidFlatInline, laidDir, 2, uint32(dir), 0)
	copy(head[bs+32:], laidDirBlock(ents))

	return head
}

// TestContractCloseReadsInlineEndsInAnyOrderOnceEach: the order a copy
// meets inline ends in is the image's to choose, and one that never lets
// an end follow another costs Close a read of each end, and no window:
// three hundred files of 100 bytes whose inodes lie in the reverse of
// their names are read in under twice their thirty thousand bytes, where
// in the order of their names a few reads take them all, ends of 5 bytes
// with more between them than in them as well. And every file has its own
// bytes whatever read served it: ends of one byte, 64 apart, put one of them
// a byte past the 4 KiB a read took, which is not in that read.
func TestContractCloseReadsInlineEndsInAnyOrderOnceEach(t *testing.T) {
	t.Parallel()

	const files = 300

	for name, tc := range map[string]struct {
		size     int
		reversed bool
	}{
		"in the order of their names":        {100, false},
		"small, in the order of their names": {5, false},
		"of one byte, in that order":         {1, false},
		"in the reverse of it":               {100, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			size, reversed := tc.size, tc.reversed
			src := &countedReads{data: laidInlineFiles(files, size, reversed)}

			from, err := erofs.Open(src)
			if err != nil {
				t.Fatal(err)
			}

			out := &testBuffer{}
			w := erofs.Create(out)

			if err = w.CopyFrom(from, erofs.MetadataOnly()); err != nil {
				t.Fatal(err)
			}

			src.calls, src.bytes = 0, 0

			if err = w.Close(); err != nil {
				t.Fatal(err)
			}

			switch {
			case reversed && src.bytes > 2*files*size:
				t.Errorf(
					"Close read %d bytes of the image for %d of inline ends; want under twice that",
					src.bytes,
					files*size,
				)
			case !reversed && src.calls > files/10:
				t.Errorf(
					"Close read the image %d times for %d ends that follow each other; want under %d",
					src.calls,
					files,
					files/10,
				)
			}

			fsys := openWithDevices(t, out.Bytes(), src.data)

			for i := range files {
				got, err := fs.ReadFile(fsys, fmt.Sprintf("f%03d", i))
				if err != nil || !bytes.Equal(got, inlineContent(i, size)) {
					t.Errorf("f%03d reads %q, %v; want its own %d bytes", i, got, err, size)
				}
			}
		})
	}
}

// TestContractMetadataOnlyCopyRefusesAnInlineEndPastTheImage: an inode
// that ends on the image's last byte has its inline end after the image.
// The copy reads no end before Close, and refuses that one all the same
// where it copies the file, ErrCorrupt, whether its reader ends with the
// image or goes on with bytes that are not the image's.
func TestContractMetadataOnlyCopyRefusesAnInlineEndPastTheImage(t *testing.T) {
	t.Parallel()

	const bs = 4096

	// The root and its one entry, then the file's inode in the last 32
	// bytes of the second and last block.
	img := make([]byte, 2*bs)
	laidSuper(img, 12, 2)

	dir := laidDirBlock([]laidDirent{{nid: (bs - 32) / 32, ftype: laidFtReg, name: "f"}})
	laidCompact(img[bs:], laidFlatInline, laidDir, 2, uint32(len(dir)), 0)
	copy(img[bs+32:], dir)
	laidCompact(img[2*bs-32:], laidFlatInline, laidReg, 1, 100, 0)

	for name, source := range map[string][]byte{
		"a reader that ends with the image": img,
		"a reader that goes on":             append(bytes.Clone(img), bytes.Repeat([]byte("not the image's "), 60)...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			from, err := erofs.Open(bytes.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}

			out := &sink{}
			w := erofs.Create(out)

			if err := w.CopyFrom(from, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrCorrupt) {
				t.Fatalf("CopyFrom: %v; want ErrCorrupt", err)
			}
		})
	}
}
