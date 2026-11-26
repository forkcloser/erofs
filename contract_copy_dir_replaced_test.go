package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// replacedTree is what the tests below expect of a tree whose directory a
// became a file: a reads as the file, and nothing is found under it.
func replacedTree(t *testing.T, fsys fs.FS, want string) {
	t.Helper()

	if got, err := fs.ReadFile(fsys, "a"); err != nil || string(got) != want {
		t.Errorf("a reads %q, %v; want %q", got, err, want)
	}

	if _, err := fs.Stat(fsys, "a/b"); err == nil {
		t.Error("a/b is still found under a, now a file")
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if e.Name() == "a" && e.IsDir() {
			t.Error("the root lists a as a directory")
		}
	}
}

// TestContractCopyReplacingADirectoryByAFileDropsItsEntries: a later copy
// that has a file where the Writer holds a directory replaces it, and the
// directory's entries go with it. They used to stay registered under the
// file: the Writer's Stat still found them.
func TestContractCopyReplacingADirectoryByAFileDropsItsEntries(t *testing.T) {
	t.Parallel()

	lower := fstest.MapFS{"a/b": {Data: []byte("b")}, "a/d/e": {Data: []byte("e")}}
	upper := fstest.MapFS{"a": {Data: []byte("now a file")}}

	image := func(t *testing.T, tree fs.FS) []byte {
		t.Helper()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		if err := errors.Join(w.CopyFrom(tree), w.Close()); err != nil {
			t.Fatal(err)
		}

		return out.Bytes()
	}

	t.Run("by the walk", func(t *testing.T) {
		t.Parallel()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		if err := errors.Join(w.CopyFrom(lower), w.CopyFrom(upper)); err != nil {
			t.Fatal(err)
		}

		for _, name := range []string{"/a/b", "/a/d", "/a/d/e"} {
			if _, err := w.Stat(name); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the Writer's Stat(%s): %v; want ErrNotExist", name, err)
			}
		}

		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		replacedTree(t, openWithDevices(t, out.Bytes()), "now a file")
	})

	t.Run("by the copy of an image", func(t *testing.T) {
		t.Parallel()

		lowerImage, upperImage := image(t, lower), image(t, upper)

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		for _, layer := range [][]byte{lowerImage, upperImage} {
			source, err := erofs.Open(bytes.NewReader(layer))
			if err != nil {
				t.Fatal(err)
			}

			if err := w.CopyFrom(source, erofs.MetadataOnly()); err != nil {
				t.Fatal(err)
			}
		}

		for _, name := range []string{"/a/b", "/a/d", "/a/d/e"} {
			if _, err := w.Stat(name); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the Writer's Stat(%s): %v; want ErrNotExist", name, err)
			}
		}

		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		replacedTree(t, openWithDevices(t, out.Bytes(), lowerImage, upperImage), "now a file")
	})
}

// TestContractCopyReplacingADirectoryKeepsAHardLinkIntoIt: a name outside
// the directory that is a hard link to a file inside it keeps the file when
// the directory is replaced. Close used to fail: the link's target was a
// name the Writer still held and could no longer place.
func TestContractCopyReplacingADirectoryKeepsAHardLinkIntoIt(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/a/b", []byte("b"))

	if err := w.Link("/a/b", "/c"); err != nil {
		t.Fatal(err)
	}

	if err := w.CopyFrom(fstest.MapFS{"a": {Data: []byte("now a file")}}); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fsys := openWithDevices(t, out.Bytes())
	replacedTree(t, fsys, "now a file")

	if got, err := fs.ReadFile(fsys, "c"); err != nil || string(got) != "b" {
		t.Errorf("c reads %q, %v; want %q", got, err, "b")
	}
}
