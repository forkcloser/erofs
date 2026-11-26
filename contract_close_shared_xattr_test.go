package erofs_test

import (
	"fmt"
	"testing"

	"github.com/forkcloser/erofs"
)

// closeAllocatesWithAttrs is the bytes Close allocates for a Writer given
// 9999 files of ten bytes, each with that many attributes, the value of
// each the same on three files and on no other.
func closeAllocatesWithAttrs(t *testing.T, attrs int) uint64 {
	t.Helper()

	w := erofs.Create(&sink{})

	for f := range 9999 {
		name := fmt.Sprintf("/f%05d", f)
		writeFile(t, w, name, []byte("ten bytes."))

		for a := range attrs {
			if err := w.Setxattr(name, fmt.Sprintf("user.a%03d", a), fmt.Sprintf("v%06d", f/3)); err != nil {
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

// TestContractCloseAllocatesLittleForASharedAttribute: to choose what goes
// to the shared xattr area and to write the attributes, Close allocates
// under 96 bytes for each attribute of each file in the tree that costs it
// most: every attribute shared, by the three files that sharing takes and
// no more, so that there is a shared attribute for every three carried. It
// allocated 157: a table grown as it filled to count the attributes, a
// second one for where they are in the area, a list grown the same way,
// and four bytes for each entry written.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractCloseAllocatesLittleForASharedAttribute(t *testing.T) {
	const (
		attrs  = 20
		budget = 96
	)

	bare := closeAllocatesWithAttrs(t, 0)
	with := closeAllocatesWithAttrs(t, attrs)
	each := (with - bare) / (9999 * attrs)

	if each >= budget {
		t.Errorf(
			"Close allocated %d bytes with %d attributes on each of 9999 files and %d with none: %d for each attribute of each file; want under %d",
			with,
			attrs,
			bare,
			each,
			budget,
		)
	}
}
