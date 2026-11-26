package erofs_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// labeled is an image of 64 KiB blocks whose files "f0".. each carry
// security.selinux with the value of that length, as a reference to the
// shared area or beside the inode, and the image as a source that counts.
func labeled(shared bool, lengths ...int) *laidImage {
	l := corpus.Layout{BlockBits: 16}

	for i, length := range lengths {
		x := corpus.AXattr{Index: 6, Suffix: "selinux", Value: bytes.Repeat([]byte{byte('a' + i)}, length)}
		e := corpus.AEntry{Name: fmt.Sprintf("f%d", i), Mode: 0o100644, Nlink: 1}

		if shared {
			l.Shared = append(l.Shared, x)
			e.SharedIDs = []int{i}
		} else {
			e.Inline = []corpus.AXattr{x}
		}

		l.Entries = append(l.Entries, e)
	}

	data := corpus.Assemble(l)

	return &laidImage{head: data, size: int64(len(data))}
}

// statLabel is the security.selinux of name, and what Stat read for it.
func statLabel(t *testing.T, src *laidImage, name string) (label string, calls, read int64) {
	t.Helper()

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.calls, src.bytes = 0, 0

	info, err := fs.Stat(fsys, name)
	if err != nil {
		t.Fatal(err)
	}

	st, ok := info.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("Sys() is %T", info.Sys())
	}

	return st.Xattrs["security.selinux"], src.calls, src.bytes
}

// TestContractSharedXattrReadsTheEntryNotItsBlock: an attribute an inode
// names in the shared area costs the Stat of that inode one read more than
// the same attribute kept beside the inode, of the entry and not of a
// block: a block of 64 KiB for a label of 26 bytes, for each inode that
// carries it, was 66 KiB read an inode.
func TestContractSharedXattrReadsTheEntryNotItsBlock(t *testing.T) {
	t.Parallel()

	const label = 26

	want := string(bytes.Repeat([]byte{'a'}, label))

	inlineLabel, inlineCalls, inlineRead := statLabel(t, labeled(false, label), "f0")
	sharedLabel, sharedCalls, sharedRead := statLabel(t, labeled(true, label), "f0")

	if inlineLabel != want || sharedLabel != want {
		t.Fatalf("security.selinux: %q beside the inode, %q shared; want %q", inlineLabel, sharedLabel, want)
	}

	if sharedCalls != inlineCalls+1 {
		t.Errorf("Stat took %d reads with the label shared and %d with it beside the inode; want one more",
			sharedCalls, inlineCalls)
	}

	if more := sharedRead - inlineRead; more > 256 {
		t.Errorf("Stat read %d bytes more with the label shared (%d against %d); want no more than 256",
			more, sharedRead, inlineRead)
	}
}

// TestContractSharedXattrOfAnyLengthIsRead: an entry is read whole whatever
// of it the first read took: one that ends with that read, a byte before
// and a byte after, and one as long as a value gets.
func TestContractSharedXattrOfAnyLengthIsRead(t *testing.T) {
	t.Parallel()

	// The first read takes the 4-byte header, the 7 of "selinux" and 117 of
	// the value.
	lengths := []int{0, 1, 116, 117, 118, 300, 65535}
	src := labeled(true, lengths...)

	for i, length := range lengths {
		got, _, _ := statLabel(t, src, fmt.Sprintf("f%d", i))

		if want := string(bytes.Repeat([]byte{byte('a' + i)}, length)); got != want {
			t.Errorf("a shared value of %d bytes reads as %d bytes, %.20q...", length, len(got), got)
		}
	}
}
