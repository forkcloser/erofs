package erofs_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// closeAllocatesForLinks is the bytes Close allocates for a Writer given
// ten thousand symbolic links, each to a target of its own of that many
// bytes.
func closeAllocatesForLinks(t *testing.T, target int) uint64 {
	t.Helper()

	w := erofs.Create(&sink{})
	tail := strings.Repeat("t", target-6)

	for i := range 10000 {
		if err := w.Symlink(fmt.Sprintf("%05d/%s", i, tail), fmt.Sprintf("/s%05d", i)); err != nil {
			t.Fatal(err)
		}
	}

	before := allocatedSoFar()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return allocatedSoFar() - before
}

// TestContractCloseAllocatesNothingForALinksTarget: the Writer holds a
// symbolic link's target, and Close writes it from there: what Close
// allocates for ten thousand links is the same within a kilobyte a link
// whether their targets are of a hundred bytes or of three thousand. A
// target that does not fit beside its inode, as most of three thousand
// bytes do not, was copied to be written: 3345 bytes a link against 460.
//
//nolint:paralleltest // measures the process's allocations; a parallel test's would count with them
func TestContractCloseAllocatesNothingForALinksTarget(t *testing.T) {
	short := closeAllocatesForLinks(t, 100)
	long := closeAllocatesForLinks(t, 3000)

	if long > short+10000*1024 {
		t.Errorf(
			"Close allocated %d bytes for ten thousand links to targets of 3000 bytes and %d to targets of 100; want the same within a kilobyte a link",
			long,
			short,
		)
	}
}
