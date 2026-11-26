package erofs_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/go-erofs"
)

// sourceFiles is how many files the trees of these tests have.
const sourceFiles = 4000

// underDir is sourceFiles files of that data in one directory of that name.
func underDir(dir, data string) fstest.MapFS {
	tree := fstest.MapFS{}
	for i := range sourceFiles {
		tree[fmt.Sprintf("%s/f%05d", dir, i)] = &fstest.MapFile{Data: []byte(data), Mode: 0o644}
	}

	return tree
}

// heldByCopyOf is what a Writer holds once it has copied src, until Close.
func heldByCopyOf(t *testing.T, src fstest.MapFS) int64 {
	t.Helper()

	before := liveHeap()
	w := erofs.Create(&sink{})

	if err := w.CopyFrom(src); err != nil {
		t.Fatal(err)
	}

	held := liveHeap() - before

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	runtime.KeepAlive(src)

	return held
}

// TestContractCopyHoldsAFilesPathOnce: a file copied with its data is read
// at Close, by its name in the source, and until then the Writer holds that
// name once: it is the entry's own path. Four thousand files under a
// directory whose name is 200 bytes longer cost the copy under 300 bytes a
// file more, where the path held twice is over 400.
//
//nolint:paralleltest // measures the process's live heap; a parallel test's would count in it
func TestContractCopyHoldsAFilesPathOnce(t *testing.T) {
	short := heldByCopyOf(t, underDir("d", "ten bytes."))
	long := heldByCopyOf(t, underDir("d"+strings.Repeat("x", 200), "ten bytes."))

	if extra := (long - short) / sourceFiles; extra > 300 {
		t.Errorf("200 bytes more of path cost the copy %d bytes a file; want them held once, under 300", extra)
	}
}

// TestContractCopyHoldsOneThingForAFileItReadsAtClose: what a copy holds
// for a file whose data it reads at Close, over what it holds for an empty
// one, is where to open it, once: under 72 bytes a file, for the reading at
// Close and for an Open before it alike.
//
//nolint:paralleltest // measures the process's live heap; a parallel test's would count in it
func TestContractCopyHoldsOneThingForAFileItReadsAtClose(t *testing.T) {
	empty := heldByCopyOf(t, underDir("d", ""))
	read := heldByCopyOf(t, underDir("d", "ten bytes."))

	if extra := (read - empty) / sourceFiles; extra > 72 {
		t.Errorf("a file read at Close costs the copy %d bytes more than an empty one; want under 72", extra)
	}
}
