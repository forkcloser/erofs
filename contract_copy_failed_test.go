package erofs_test

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// errDoesNotOpen is what a notOpening source answers for its one file.
var errDoesNotOpen = errors.New("notOpening: this file does not open")

// notOpening is a source one file of which does not open.
type notOpening struct {
	fs.FS

	name string
}

func (s notOpening) Open(name string) (fs.File, error) {
	if name == s.name {
		return nil, errDoesNotOpen
	}

	return s.FS.Open(name)
}

// flatTree is n files of ten bytes, "f00000" and on.
func flatTree(n int) fstest.MapFS {
	tree := fstest.MapFS{}
	for i := range n {
		tree[fmt.Sprintf("f%05d", i)] = &fstest.MapFile{Data: []byte("ten bytes."), Mode: 0o644}
	}

	return tree
}

// TestContractCloseAfterAFailedCopyReturnsItsError: a CopyFrom that fails
// once it has begun has left a part of its source in the tree, and that
// part is not an image: Close returns the copy's error and writes nothing,
// and every call after the copy returns the same error.
func TestContractCloseAfterAFailedCopyReturnsItsError(t *testing.T) {
	t.Parallel()

	// Ten files, the last of which does not open: nine are in the tree
	// when the copy fails.
	out := &sink{}
	w := erofs.Create(out)

	err := w.CopyFrom(notOpening{FS: flatTree(10), name: "f00009"})
	if !errors.Is(err, errDoesNotOpen) {
		t.Fatalf("CopyFrom of a source whose last file does not open: %v", err)
	}

	if _, serr := w.Stat("/f00000"); serr != nil {
		t.Fatalf("the copy failed before it copied anything: %v", serr)
	}

	if merr := w.Mkdir("/d", 0o755); !errors.Is(merr, errDoesNotOpen) {
		t.Errorf("Mkdir after the failed copy: %v; want the copy's error", merr)
	}

	if cerr := w.CopyFrom(flatTree(1)); !errors.Is(cerr, errDoesNotOpen) {
		t.Errorf("CopyFrom after the failed copy: %v; want the copy's error", cerr)
	}

	// What reads the Writer still answers for what it holds.
	if _, serr := w.Stat("/f00008"); serr != nil {
		t.Errorf("Stat of a file copied before the failure: %v", serr)
	}

	if _, serr := w.Stat("/f00009"); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("Stat of the file that did not open: %v; want ErrNotExist", serr)
	}

	for range 2 {
		if cerr := w.Close(); !errors.Is(cerr, errDoesNotOpen) {
			t.Errorf("Close after the failed copy: %v; want the copy's error", cerr)
		}
	}

	if out.size != 0 {
		t.Errorf("Close wrote %d bytes of the part that was copied", out.size)
	}
}

// TestContractCloseAfterAFailedCopyOfAnImageReturnsItsError: the same of an
// image, by either route: one whose second file lists an attribute twice is
// refused there, after its first file was copied.
func TestContractCloseAfterAFailedCopyOfAnImageReturnsItsError(t *testing.T) {
	t.Parallel()

	twice := corpus.AXattr{Index: 1, Suffix: "k", Value: []byte("v")}
	img := corpus.Assemble(corpus.Layout{BlockBits: 12, Entries: []corpus.AEntry{
		{Name: "a", Mode: 0o100644, Nlink: 1, Data: []byte("sound")},
		{Name: "b", Mode: 0o100644, Nlink: 1, Data: []byte("not"), Inline: []corpus.AXattr{twice, twice}},
	}})

	for route, opts := range map[string][]erofs.CopyOpt{
		"with its data": nil,
		"metadata-only": {erofs.MetadataOnly()},
	} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()

			out := &sink{}
			w := erofs.Create(out)

			if err := w.CopyFrom(openWithDevices(t, img), opts...); !errors.Is(err, erofs.ErrCorrupt) {
				t.Fatalf("CopyFrom: %v; want ErrCorrupt", err)
			}

			if _, err := w.Stat("/a"); err != nil {
				t.Fatalf("the copy failed before it copied anything: %v", err)
			}

			if err := w.Close(); !errors.Is(err, erofs.ErrCorrupt) {
				t.Errorf("Close after the failed copy: %v; want the copy's error", err)
			}

			if out.size != 0 {
				t.Errorf("Close wrote %d bytes of the part that was copied", out.size)
			}
		})
	}
}

// TestContractACopyRefusedBeforeItBeginsDoesNotFailTheWriter: a CopyFrom
// refused before it copies anything, for a file still open or for an image
// of another block size than the Writer's, is that call's failure alone:
// the Writer goes on, and Close writes its image.
func TestContractACopyRefusedBeforeItBeginsDoesNotFailTheWriter(t *testing.T) {
	t.Parallel()

	t.Run("a file still open", func(t *testing.T) {
		t.Parallel()

		out := &sink{}
		w := erofs.Create(out)

		f, err := w.Create("/open")
		if err != nil {
			t.Fatal(err)
		}

		if cerr := w.CopyFrom(flatTree(3)); cerr == nil {
			t.Fatal("CopyFrom with a file still open: no error")
		}

		if err := errors.Join(f.Close(), w.CopyFrom(flatTree(3)), w.Close()); err != nil {
			t.Fatalf("the Writer after the refused copy: %v", err)
		}

		if out.size == 0 {
			t.Error("Close wrote no image")
		}
	})

	t.Run("an image of another block size", func(t *testing.T) {
		t.Parallel()

		small := &testBuffer{}
		sw := erofs.Create(small, erofs.WithBlockSize(512))
		writeFile(t, sw, "/f", []byte("in blocks of 512"))

		if err := sw.Close(); err != nil {
			t.Fatal(err)
		}

		for route, opts := range map[string][]erofs.CopyOpt{
			"with its data": nil,
			"metadata-only": {erofs.MetadataOnly()},
		} {
			out := &sink{}
			w := erofs.Create(out, erofs.WithBlockSize(4096))

			if cerr := w.CopyFrom(openWithDevices(t, small.Bytes()), opts...); cerr == nil {
				t.Fatalf("%s: CopyFrom of an image of 512-byte blocks into a Writer of 4096: no error", route)
			}

			if err := errors.Join(w.CopyFrom(flatTree(3)), w.Close()); err != nil {
				t.Fatalf("%s: the Writer after the refused copy: %v", route, err)
			}

			if out.size == 0 {
				t.Errorf("%s: Close wrote no image", route)
			}
		}
	})
}
