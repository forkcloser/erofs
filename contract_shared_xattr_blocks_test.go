package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// patterned is n bytes no two blocks of which are alike.
func patterned(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('a' + i%23 + i/512%3)
	}

	return out
}

// sharedAcrossBlocks is an image of two files that share two attributes:
// one whose name suffix and value are given, then security.after.
func sharedAcrossBlocks(blockBits uint8, suffix string, value []byte) []byte {
	entry := func(name string) corpus.AEntry {
		return corpus.AEntry{
			Name: name, Mode: 0o100644, Nlink: 1, Mtime: corpus.AssembledBuildTime,
			Data: []byte("x"), SharedIDs: []int{0, 1},
		}
	}

	return corpus.Assemble(corpus.Layout{
		BlockBits:  blockBits,
		NoChecksum: true,
		Shared: []corpus.AXattr{
			{Index: 1, Suffix: suffix, Value: value},
			{Index: 6, Suffix: "after", Value: []byte("the next entry")},
		},
		Entries: []corpus.AEntry{entry("a"), entry("b")},
	})
}

// TestContractSharedXattrIsReadAcrossBlocks: an entry of the shared xattr
// area runs on for as long as its header says, across blocks, as the kernel
// reads it (xattr.c: erofs_xattr_copy_to_buffer). One longer than a block
// was refused, ErrCorrupt, with every file that shares it.
func TestContractSharedXattrIsReadAcrossBlocks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		blockBits uint8
		suffix    string
		value     int
	}{
		{9, "big", 400},   // within one block: the common case
		{9, "big", 505},   // header, name and value fill 512 bytes exactly
		{9, "big", 506},   // one byte into the next 512
		{9, "big", 600},   // well into it
		{9, "big", 65535}, // the longest value the format holds
		{9, string(bytes.Repeat([]byte("n"), 255)), 300}, // the name itself crosses
		{12, "big", 5000},
		{16, "big", 65535},
	} {
		t.Run(fmt.Sprintf("%d-byte blocks, a name of %d and a value of %d", 1<<tc.blockBits, len(tc.suffix), tc.value),
			func(t *testing.T) {
				t.Parallel()

				value := patterned(tc.value)
				img := sharedAcrossBlocks(tc.blockBits, tc.suffix, value)
				name := "user." + tc.suffix

				// The image is what it is meant to be: the reference reads
				// the attribute from both files.
				ref, err := erofsspec.Parse(img)
				if err != nil {
					t.Fatalf("the reference: %v", err)
				}

				seen := 0

				err = ref.Walk(func(e erofsspec.Entry) error {
					for _, x := range e.Inode.Xattrs {
						if x.Name() == name && bytes.Equal(x.Value, value) {
							seen++
						}
					}

					return nil
				})
				if err != nil || seen != 2 {
					t.Fatalf("the reference reads the attribute on %d files, %v; want 2", seen, err)
				}

				fsys, err := erofs.Open(bytes.NewReader(img))
				if err != nil {
					t.Fatal(err)
				}

				for _, file := range []string{"a", "b"} {
					fi, err := fs.Stat(fsys, file)
					if err != nil {
						t.Errorf("Stat(%s): %v", file, err)

						continue
					}

					xattrs := fi.Sys().(*erofs.Stat).Xattrs
					if got := xattrs[name]; got != string(value) {
						t.Errorf("%s: the attribute reads %d bytes, want the %d written", file, len(got), len(value))
					}

					if got := xattrs["security.after"]; got != "the next entry" {
						t.Errorf("%s: security.after reads %q", file, got)
					}
				}
			})
	}
}

// TestContractSharedXattrPastTheImageIsCorrupt: an entry whose header claims
// more than the image holds is ErrCorrupt, not a value cut short.
func TestContractSharedXattrPastTheImageIsCorrupt(t *testing.T) {
	t.Parallel()

	value := patterned(600)
	img := sharedAcrossBlocks(9, "big", value)

	// e_name_len, e_name_index, e_value_size: the four bytes before the name.
	at := bytes.Index(img, append([]byte("big"), value[:32]...))
	if at < 4 {
		t.Fatal("the entry is not found in the image")
	}

	binary.LittleEndian.PutUint16(img[at-2:], 65535)

	if at+65535 < len(img) {
		t.Fatalf("an image of %d bytes holds 65535 bytes from %d: the test tests nothing", len(img), at)
	}

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fs.Stat(fsys, "a"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("Stat = %v; want ErrCorrupt", err)
	}
}
