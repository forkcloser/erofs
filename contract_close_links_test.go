package erofs_test

import (
	"fmt"
	"testing"

	"github.com/forkcloser/erofs"
)

// closeAllocates is the bytes Close allocates for a Writer given as many
// files of ten bytes as asked, each under that many names in all.
func closeAllocates(t *testing.T, files, names int) uint64 {
	t.Helper()

	w := erofs.Create(&sink{})

	for f := range files {
		first := fmt.Sprintf("/f%05d-0", f)
		writeFile(t, w, first, []byte("ten bytes."))

		for n := 1; n < names; n++ {
			if err := w.Link(first, fmt.Sprintf("/f%05d-%d", f, n)); err != nil {
				t.Fatal(err)
			}
		}
	}

	before := allocatedSoFar()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return allocatedSoFar() - before
}

// TestContractCloseAllocatesNoMoreForAFurtherNameThanForAFile: a file's
// further name is an entry of a directory and nothing else, no inode and no
// data, so Close allocates no more for ten thousand names of a thousand
// files than for ten thousand files of one name each. It allocated half as
// much again: each further name had its place in the slab of entries, left
// empty, and an entry of its own beside it.
//
//nolint:paralleltest // measures the process's allocations; a parallel test's would count with them
func TestContractCloseAllocatesNoMoreForAFurtherNameThanForAFile(t *testing.T) {
	const names = 10000

	files := closeAllocates(t, names, 1)
	links := closeAllocates(t, names/10, 10)

	if links > files {
		t.Errorf(
			"Close allocated %d bytes for %d names of %d files and %d for as many files of one name; want no more for the names",
			links,
			names,
			names/10,
			files,
		)
	}
}
