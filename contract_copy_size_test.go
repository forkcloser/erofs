package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// copyWithData is the copy of src, with its data, as an image and as the tree it
// opens to.
func copyWithData(t *testing.T, src *laidImage) ([]byte, fs.FS) {
	t.Helper()

	img, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	out := &testBuffer{}

	w := erofs.Create(out)
	if err = w.CopyFrom(img); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := erofs.Open(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	return out.Bytes(), got
}

// TestContractCopyStoresADataBlockForEachChunkThatNamesIt: a copy with its
// data writes each regular file at its logical size, whatever blocks its
// chunks share in the source: a file of 512 chunks that all name one block,
// in an image of 8192 bytes, is two megabytes of the copy.
func TestContractCopyStoresADataBlockForEachChunkThatNamesIt(t *testing.T) {
	t.Parallel()

	const (
		bs     = 4096
		chunks = 512
	)

	// "f": a block map of zeros, every chunk block 0 of the image.
	src := laidChunkedFile(chunks, 0)

	image, got := copyWithData(t, src)

	if len(image) < chunks*bs {
		t.Errorf("the copy of a %d byte image is %d bytes, under the %d of its file", src.size, len(image), chunks*bs)
	}

	data, err := fs.ReadFile(got, "f")
	if err != nil {
		t.Fatal(err)
	}

	// Block 0 of the source, once a chunk.
	if want := bytes.Repeat(src.whole()[:bs], chunks); !bytes.Equal(data, want) {
		t.Errorf("the copy's file is %d bytes and not the source's block 0, %d times over", len(data), chunks)
	}
}

// TestContractCopyStoresOnceAnAttributeItsFilesShare: an attribute a
// source image stores once, in its shared area, and that many of its files
// name, is in the copy once too, in the copy's own shared area: 64 files
// that name one value of 60000 bytes are a copy of under twice its source,
// a few blocks, and each of them has the value.
func TestContractCopyStoresOnceAnAttributeItsFilesShare(t *testing.T) {
	t.Parallel()

	const (
		files  = 64
		length = 60000
	)

	value := bytes.Repeat([]byte{'s'}, length)

	l := corpus.Layout{
		BlockBits: 16,
		Shared:    []corpus.AXattr{{Index: 6, Suffix: "selinux", Value: value}},
	}

	for i := range files {
		l.Entries = append(l.Entries, corpus.AEntry{
			Name: fmt.Sprintf("f%02d", i), Mode: 0o100644, Nlink: 1, SharedIDs: []int{0},
		})
	}

	data := corpus.Assemble(l)
	src := &laidImage{head: data, size: int64(len(data))}

	if src.size >= 4*length {
		t.Fatalf("the source is %d bytes: it does not store the value once", src.size)
	}

	image, got := copyWithData(t, src)

	if int64(len(image)) > 2*src.size {
		t.Errorf("the copy of a %d byte image is %d bytes; want the value once, under twice the source",
			src.size, len(image))
	}

	// Metadata-only, the value is once in the copy as well.
	sink := &testBuffer{}
	mw := erofs.Create(sink)

	if err := errors.Join(mw.CopyFrom(openWithDevices(t, data), erofs.MetadataOnly()), mw.Close()); err != nil {
		t.Fatal(err)
	}

	if int64(len(sink.Bytes())) > 2*src.size {
		t.Errorf("the metadata-only copy of a %d byte image is %d bytes; want the value once, under twice the source",
			src.size, len(sink.Bytes()))
	}

	for i := range files {
		name := fmt.Sprintf("f%02d", i)

		info, err := fs.Stat(got, name)
		if err != nil {
			t.Fatal(err)
		}

		st, ok := info.Sys().(*erofs.Stat)
		if !ok {
			t.Fatalf("Sys() is %T", info.Sys())
		}

		if st.Xattrs["security.selinux"] != string(value) {
			t.Fatalf(
				"%s: security.selinux is %d bytes, not the source's value",
				name,
				len(st.Xattrs["security.selinux"]),
			)
		}
	}
}

// laidSharedListing is an image of 4 KiB blocks whose root holds dirs
// directories, "d000".., that all store their entries in one block, the
// image's last: ".", "..", and names names, "n000".., of one empty regular
// file. The image is four blocks.
func laidSharedListing(dirs, names int) *laidImage {
	const bs = 4096

	head := make([]byte, 4*bs)

	rootEnts := []laidDirent{{0, laidFtDir, "."}, {0, laidFtDir, ".."}}

	listing := []laidDirent{{0, laidFtDir, "."}, {0, laidFtDir, ".."}}

	root := laidDirBlock(append(rootEnts, make([]laidDirent, dirs)...))

	// The directories' inodes follow the root's inline entries, then the file's.
	first := uint64((32 + len(root) + dirs*4 + 31) / 32)
	file := first + uint64(dirs)

	for i := range dirs {
		rootEnts = append(rootEnts, laidDirent{first + uint64(i), laidFtDir, fmt.Sprintf("d%03d", i)})
	}

	for i := range names {
		listing = append(listing, laidDirent{file, laidFtReg, fmt.Sprintf("n%03d", i)})
	}

	root = laidDirBlock(rootEnts)
	shared := laidDirBlock(listing)

	laidCompact(head[bs:], laidFlatInline, laidDir, uint16(2+dirs), uint32(len(root)), 0)
	copy(head[bs+32:], root)

	for i := range dirs {
		laidCompact(head[bs+int(first)*32+i*32:], laidFlatPlain, laidDir, 2, uint32(len(shared)), 3)
	}

	laidCompact(head[bs+int(file)*32:], laidFlatPlain, laidReg, 1, 0, 0)
	copy(head[3*bs:], shared)

	laidSuper(head, 12, 4)

	return &laidImage{head: head, size: int64(len(head))}
}

// TestContractCopyStoresANameForEachDirectoryThatListsIt: entries a source
// image stores once, in a block that many of its directories name as
// theirs, are in the copy for each of them: 100 directories over one block
// of 200 names are 20000 names of the copy, from an image of four blocks.
func TestContractCopyStoresANameForEachDirectoryThatListsIt(t *testing.T) {
	t.Parallel()

	const (
		dirs  = 100
		names = 200
	)

	src := laidSharedListing(dirs, names)

	image, got := copyWithData(t, src)

	// A name of the copy is its entry, twelve bytes and the name's four.
	if len(image) < dirs*names*16 {
		t.Errorf("the copy of a %d byte image is %d bytes, under the %d of its entries",
			src.size, len(image), dirs*names*16)
	}

	for i := range dirs {
		entries, err := fs.ReadDir(got, fmt.Sprintf("d%03d", i))
		if err != nil {
			t.Fatal(err)
		}

		if len(entries) != names {
			t.Fatalf("d%03d lists %d names, not %d", i, len(entries), names)
		}
	}
}

// TestContractCopyStopsWhereItsWriterDoes: what bounds a copy is what it is
// written to: a destination that refuses a megabyte fails the copy of an
// 8192 byte image whose file is two, with the destination's own error.
func TestContractCopyStopsWhereItsWriterDoes(t *testing.T) {
	t.Parallel()

	img, err := erofs.Open(laidChunkedFile(512, 0))
	if err != nil {
		t.Fatal(err)
	}

	w := erofs.Create(&testBuffer{limit: 1 << 20})

	err = w.CopyFrom(img)
	if err == nil {
		err = w.Close()
	}

	if !errors.Is(err, errTestBufferFull) {
		t.Errorf("the copy into a destination of a megabyte: %v, want the destination's refusal", err)
	}
}
